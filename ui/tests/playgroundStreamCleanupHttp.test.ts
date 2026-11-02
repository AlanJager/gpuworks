/** Native HTTP controls for protocol completion, abort and reader cleanup. */
import assert from 'node:assert/strict';
import { createServer, type ServerResponse } from 'node:http';
import { once } from 'node:events';
import type { Socket } from 'node:net';
import { setImmediate } from 'node:timers/promises';
import test from 'node:test';
import { streamChat } from '../src/pages/playground/api';

const PATH = '/api/v1/playground/v1/chat/completions';
const PARAMETERS = { enabled: {}, values: {}, systemPrompt: '', extraJSON: '' };
const MESSAGES = [{ role: 'user' as const, content: 'local cleanup probe' }];
const USAGE = { prompt_tokens: 16, completion_tokens: 8, total_tokens: 24 };
const ZERO = { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 };
const ERROR = 'upstream primary failure';
const frame = (value: unknown) => `data: ${JSON.stringify(value)}\n\n`;

for (const mode of ['done-held', 'abort-held', 'error-held', 'done-no-usage', 'done-zero-usage', 'done-crlf-writes'] as const) {
  test(`native stream cleanup: ${mode}`, { timeout: 2500, concurrency: false }, async (t) => {
    const content = Promise.withResolvers<void>();
    const responseClosed = Promise.withResolvers<void>();
    const controller = new AbortController();
    const handlers = new Set<Promise<void>>();
    const sockets = new Set<Socket>();
    const responses = new Set<ServerResponse>();
    const errors: unknown[] = [];
    const deltas: string[] = [];
    const usages: unknown[] = [];
    let active = 0, serverEnded = false, observedClose = false, writes = 0;
    const interrupt = () => { controller.abort(); content.resolve(); };
    t.signal.addEventListener('abort', interrupt, { once: true });
    const server = createServer((request, response) => {
      active += 1;
      responses.add(response);
      response.on('close', () => { observedClose = true; responseClosed.resolve(); });
      const task = (async () => {
        assert.equal(request.method, 'POST');
        assert.equal(request.url, PATH);
        assert.equal(request.headers['content-type'], 'application/json');
        const chunks: Buffer[] = [];
        for await (const chunk of request) chunks.push(Buffer.from(chunk));
        assert.deepEqual(JSON.parse(Buffer.concat(chunks).toString('utf8')),
          { model: 'local-model', messages: MESSAGES, stream: true });
        response.writeHead(200, { 'Content-Type': 'text/event-stream' });
        const write = (value: string) => { writes += 1; response.write(value); };
        write(frame({ choices: [{ delta: { content: 'partial' }, finish_reason: null }] }));
        await content.promise;
        if (mode === 'abort-held' || controller.signal.aborted) {
          await responseClosed.promise;
          return;
        }
        if (mode === 'error-held') {
          write(frame({ error: { message: ERROR } }));
          await responseClosed.promise;
          return;
        }
        write(frame({ choices: [{ delta: {}, finish_reason: 'stop' }] }));
        const usage = mode === 'done-no-usage' ? undefined : mode === 'done-zero-usage' ? ZERO : USAGE;
        if (usage) write(frame({ choices: [], usage }));
        if (mode === 'done-crlf-writes') {
          // Distinct HTTP writes are recorded, not assumed to be fetch-read boundaries.
          for (const piece of ['data: [DO', 'NE]\r', '\n\r', '\n']) {
            write(piece);
            await setImmediate();
          }
        } else write('data: [DONE]\n\n');
        if (mode === 'done-held') {
          await responseClosed.promise; // No response.end(): only reader cancellation can release this.
          return;
        }
        serverEnded = true;
        response.end();
      })().catch((error) => { errors.push(error); response.destroy(error as Error); })
        .finally(() => { active -= 1; responses.delete(response); });
      handlers.add(task);
      void task.finally(() => handlers.delete(task));
    });
    server.on('connection', (socket) => {
      sockets.add(socket);
      socket.on('close', () => sockets.delete(socket));
    });
    const nativeFetch = globalThis.fetch;
    let nativeResponse: Response | undefined;
    try {
      server.listen(0, '127.0.0.1');
      await once(server, 'listening');
      const address = server.address();
      assert.ok(address && typeof address !== 'string');
      const base = `http://127.0.0.1:${address.port}`;
      globalThis.fetch = (input, init) => {
        assert.equal(input, PATH);
        return nativeFetch(new URL(input as string, base), init).then((response) => {
          nativeResponse = response;
          return response;
        });
      };
      const outcome = await streamChat('local-model', MESSAGES, PARAMETERS, [], {
        onContent(delta) {
          deltas.push(delta);
          if (mode === 'abort-held') controller.abort();
          content.resolve();
        },
        onReasoning() { assert.fail('unexpected reasoning'); },
        onDelta() {},
        onUsage(usage) { usages.push(usage); },
      }, controller.signal).then(
        (value) => ({ state: 'resolved' as const, value }),
        (error: unknown) => ({ state: 'rejected' as const, error }),
      );
      await Promise.all(handlers);
      assert.deepEqual(errors, []);
      assert.deepEqual(deltas, ['partial']);
      assert.equal(nativeResponse?.body?.locked, false, 'native reader lock was not released');
      if (mode === 'abort-held' || mode === 'error-held') {
        assert.equal(outcome.state, 'rejected');
        if (outcome.state === 'rejected') {
          assert.ok(outcome.error instanceof Error);
          if (mode === 'abort-held') assert.equal(outcome.error.name, 'AbortError');
          else assert.equal(outcome.error.message, ERROR, 'reader cleanup replaced primary SSE error');
        }
        assert.deepEqual(usages, []);
      } else {
        assert.equal(outcome.state, 'resolved');
        const usage = mode === 'done-no-usage' ? undefined : mode === 'done-zero-usage' ? ZERO : USAGE;
        if (outcome.state === 'resolved') assert.deepEqual(outcome.value, { usage });
        assert.deepEqual(usages, usage ? [usage] : []);
      }
      if (mode.endsWith('-held')) {
        assert.equal(serverEnded, false);
        assert.equal(observedClose, true);
      }
      assert.equal(active, 0);
      t.diagnostic(JSON.stringify({ mode, outcome: outcome.state, observedClose, serverEnded,
        activeHandlers: active, writes, readerLocked: nativeResponse?.body?.locked,
        boundaryClaim: 'HTTP writes only; fetch read boundaries not asserted' }));
    } finally {
      globalThis.fetch = nativeFetch;
      t.signal.removeEventListener('abort', interrupt);
      interrupt();
      for (const response of responses) response.destroy();
      const closed = new Promise<void>((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
      server.closeAllConnections();
      for (const socket of sockets) socket.destroy();
      await Promise.all(handlers);
      await closed;
      assert.equal(active, 0);
      assert.equal(server.listening, false);
      t.diagnostic(JSON.stringify({ cleanup: true, activeHandlers: active, listening: server.listening,
        remainingOpenSockets: [...sockets].filter((socket) => !socket.destroyed).length }));
    }
  });
}

/** Phase1/2 diagnostic: real streamChat + native fetch + local chunked HTTP. */
import assert from 'node:assert/strict';
import { createServer, type ServerResponse } from 'node:http';
import type { Socket } from 'node:net';
import { once } from 'node:events';
import test from 'node:test';
import { streamChat } from '../src/pages/playground/api';

const CHAT = '/api/v1/playground/v1/chat/completions';
const PARAMETERS = { enabled: {}, values: {}, systemPrompt: '', extraJSON: '' };
const MESSAGES = [{ role: 'user' as const, content: 'local termination probe' }];
const BODY = { model: 'local-stream-model', messages: MESSAGES, stream: true };
const PARTIAL = 'partial answer';
const USAGE = { prompt_tokens: 16, completion_tokens: 8, total_tokens: 24 };
const frame = (value: unknown) => `data: ${JSON.stringify(value)}\n\n`;
function gate() {
  let release!: () => void;
  const promise = new Promise<void>((resolve) => { release = resolve; });
  return { promise, release };
}
for (const terminal of ['done', 'premature-eof'] as const) {
  test(`streamChat real HTTP: ${terminal}`, { timeout: 2500, concurrency: false }, async (t) => {
    const observed = gate();
    const abort = new AbortController();
    const interrupt = () => { abort.abort(); observed.release(); };
    t.signal.addEventListener('abort', interrupt, { once: true });
    const sockets = new Set<Socket>();
    const responses = new Set<ServerResponse>();
    const handlers = new Set<Promise<void>>();
    const serverErrors: unknown[] = [];
    const requests: unknown[] = [];
    const deltas: string[] = [];
    const usages: unknown[] = [];
    let active = 0, clientObservedWhileOpen = false, serverSentEnd = false;
    const server = createServer((request, response) => {
      active += 1;
      responses.add(response);
      const task = (async () => {
        assert.equal(request.method, 'POST');
        assert.equal(request.url, CHAT);
        assert.equal(request.headers['content-type'], 'application/json');
        const chunks: Buffer[] = [];
        for await (const chunk of request) chunks.push(Buffer.from(chunk));
        const body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
        requests.push(body);
        assert.deepEqual(body, BODY);
        response.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' });
        response.write(frame({ id: 'local-stream', choices: [{ index: 0, delta: { content: PARTIAL }, finish_reason: null }] }));
        // End only after the actual parser delivers a real open-body delta.
        await observed.promise;
        if (abort.signal.aborted) return;
        if (terminal === 'done') {
          response.write(frame({ id: 'local-stream', choices: [{ index: 0, delta: {}, finish_reason: 'stop' }] }));
          response.write(frame({ choices: [], usage: USAGE }));
          response.write('data: [DONE]\n\n');
        }
        serverSentEnd = true;
        response.end(); // Legal chunked HTTP EOF, not an artificially broken socket.
      })().catch((error) => {
        serverErrors.push(error);
        response.destroy(error as Error);
      }).finally(() => { active -= 1; responses.delete(response); });
      handlers.add(task);
      void task.finally(() => handlers.delete(task));
    });
    server.on('connection', (socket) => {
      sockets.add(socket);
      socket.on('close', () => sockets.delete(socket));
    });
    const nativeFetch = globalThis.fetch;
    try {
      server.listen(0, '127.0.0.1');
      await once(server, 'listening');
      const address = server.address();
      assert.ok(address && typeof address !== 'string');
      const base = `http://127.0.0.1:${address.port}`;
      // Delegate URL resolution only; real HTTP response/body/reader remain native.
      globalThis.fetch = (input, init) => {
        assert.equal(typeof input, 'string');
        assert.equal(input, CHAT);
        return nativeFetch(new URL(input as string, base), init);
      };
      const outcome = await streamChat(BODY.model, MESSAGES, PARAMETERS, [], {
        onContent(delta) {
          clientObservedWhileOpen = !serverSentEnd;
          deltas.push(delta);
          observed.release();
        },
        onReasoning() { assert.fail('unexpected reasoning'); },
        onUsage(usage) { usages.push(usage); },
        onDelta() {},
      }, abort.signal).then(
        (value) => ({ state: 'resolved' as const, value }),
        (error: unknown) => ({ state: 'rejected' as const, error }),
      );
      await Promise.all(handlers);
      assert.deepEqual(serverErrors, []);
      assert.deepEqual(requests, [BODY]);
      assert.equal(clientObservedWhileOpen, true);
      assert.deepEqual(deltas, [PARTIAL]);
      assert.equal(serverSentEnd, true);
      t.diagnostic(JSON.stringify({ terminal, actual: outcome.state, deltas, usages,
        firstDeltaWhileBodyOpen: clientObservedWhileOpen, serverSentEnd, activeHandlers: active }));
      if (terminal === 'done') {
        assert.equal(outcome.state, 'resolved');
        if (outcome.state === 'resolved') assert.deepEqual(outcome.value, { usage: USAGE });
        assert.deepEqual(usages, [USAGE]);
      } else {
        assert.deepEqual(usages, []);
        assert.equal(outcome.state, 'rejected',
          'partial assistant delta with finish_reason=null and no [DONE]/usage must not resolve as successful completion');
        if (outcome.state === 'rejected') {
          assert.ok(outcome.error instanceof Error);
          assert.equal(outcome.error.message, '__playground_stream_interrupted__');
        }
      }
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

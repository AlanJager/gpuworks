// The connection-test requests of ui/src/api.ts (testProviderConnection,
// testSavedProviderConnection): what goes on the wire, how the POSTed event
// stream is read, and how each way of ending settles: a done frame, a refused
// request, a stream that breaks, and an abort.
import assert from "node:assert/strict";
import { afterEach, mock, test } from "node:test";
import "./helpers/dom.ts";

const { api, ConnectionTestInterrupted } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");

type Check = import("../src/api.ts").ConnectionCheckEvent;

afterEach(() => mock.restoreAll());

const encoder = new TextEncoder();
/** A response whose body arrives in the given chunks; a chunk that is an Error breaks the stream there. */
function streamed(chunks: (string | Error)[]) {
  const pending = [...chunks];
  // One chunk per read, so what came before a break is delivered first, as on a real connection.
  const body = new ReadableStream<Uint8Array>({
    pull(controller) {
      const chunk = pending.shift();
      if (chunk === undefined) controller.close();
      else if (chunk instanceof Error) controller.error(chunk);
      else controller.enqueue(encoder.encode(chunk));
    },
  }, { highWaterMark: 0 });
  return new Response(body, { status: 200, headers: { "Content-Type": "text/event-stream" } });
}
function serve(answer: (route: string, init?: RequestInit) => Response | Promise<Response>) {
  const sent: { route: string; init?: RequestInit }[] = [];
  mock.method(globalThis, "fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
    sent.push({ route: String(input), init });
    return answer(String(input), init);
  });
  return sent;
}

test("a new account's test posts the request as JSON and reads checks and the summary from frames split anywhere", async () => {
  const sent = serve(() => streamed([
    'event: check\ndata: {"seq":1,"name":"endpoint_format","status":"ok","duration_ms":1}\n\nevent: che',
    'ck\ndata: {"seq":2,"name":"auth_request","status":"failed","duration_ms":7,"code":"PROVIDER_AUTH_FAILED","hint":"auth"}\n',
    '\nevent: done\ndata: {"ok":false,"failed":["auth_request"]}\n\n',
  ]));
  const checks: Check[] = [];
  const summary = await api.testProviderConnection({ kind: "runpod", api_key: "rk" }, (check) => checks.push(check));
  assert.deepEqual(summary, { ok: false, failed: ["auth_request"] });
  assert.deepEqual(checks.map((check) => [check.seq, check.name, check.status, check.code, check.hint]), [
    [1, "endpoint_format", "ok", undefined, undefined],
    [2, "auth_request", "failed", "PROVIDER_AUTH_FAILED", "auth"],
  ]);
  assert.equal(sent.length, 1);
  assert.equal(sent[0].route, "/api/v1/providers/test");
  assert.equal(sent[0].init?.method, "POST");
  assert.equal(sent[0].init?.credentials, "include");
  assert.equal(new Headers(sent[0].init?.headers).get("Content-Type"), "application/json");
  assert.equal(sent[0].init?.body, '{"kind":"runpod","api_key":"rk"}');
});

test("a saved account's test sends no body without an override, and the override as JSON with one", async () => {
  const sent = serve(() => streamed(['event: done\ndata: {"ok":true}\n\n']));
  assert.deepEqual(await api.testSavedProviderConnection(12, () => {}), { ok: true });
  assert.equal(sent[0].route, "/api/v1/providers/12/test");
  assert.equal(sent[0].init?.method, "POST");
  assert.equal(sent[0].init?.body, undefined, "the stored credentials as they are: no body");
  assert.equal(new Headers(sent[0].init?.headers).get("Content-Type"), null);

  await api.testSavedProviderConnection(12, () => {}, { endpoint: "https://zs.example", private_key: "new" });
  assert.equal(sent[1].route, "/api/v1/providers/12/test");
  assert.equal(sent[1].init?.body, '{"endpoint":"https://zs.example","private_key":"new"}');
  assert.equal(new Headers(sent[1].init?.headers).get("Content-Type"), "application/json");
});

test("a done frame carrying a code settles with it: the test could not run", async () => {
  serve(() => streamed(['event: done\ndata: {"ok":false,"code":"PROVIDER_TEST_UNAVAILABLE"}\n\n']));
  assert.deepEqual(await api.testSavedProviderConnection(12, () => {}), { ok: false, code: "PROVIDER_TEST_UNAVAILABLE" });
});

test("a refused request rejects with the server's product error", async () => {
  serve(() => new Response(JSON.stringify({ code: "PROVIDER_KIND_UNKNOWN", message: "unknown provider kind" }), { status: 400, headers: { "Content-Type": "application/json" } }));
  await assert.rejects(api.testProviderConnection({ kind: "nope" }, () => {}), (error: unknown) => {
    assert.ok(error instanceof APIError, String(error));
    assert.equal(error.code, "PROVIDER_KIND_UNKNOWN");
    assert.equal(error.status, 400);
    return true;
  });
});

test("a stream that ends before its summary, breaks mid-way or never connects rejects as interrupted, after delivering the checks that arrived", async () => {
  const check = 'event: check\ndata: {"seq":1,"name":"endpoint_format","status":"ok","duration_ms":1}\n\n';
  const cases: [string, () => Response | Promise<Response>][] = [
    ["clean end without a done frame", () => streamed([check])],
    ["network error mid-stream", () => streamed([check, new TypeError("terminated")])],
  ];
  for (const [name, answer] of cases) {
    serve(answer);
    const checks: Check[] = [];
    await assert.rejects(api.testSavedProviderConnection(3, (c) => checks.push(c)), ConnectionTestInterrupted, name);
    assert.deepEqual(checks.map((c) => c.name), ["endpoint_format"], name);
    mock.restoreAll();
  }
  mock.method(globalThis, "fetch", async () => { throw new TypeError("fetch failed"); });
  await assert.rejects(api.testSavedProviderConnection(3, () => {}), ConnectionTestInterrupted, "the request never reached the server");
});

test("the signal goes to fetch, and aborting it rejects with the abort, not as an interrupted test", async () => {
  let seen: AbortSignal | undefined;
  mock.method(globalThis, "fetch", (_input: RequestInfo | URL, init?: RequestInit) => new Promise<Response>((_resolve, reject) => {
    seen = init?.signal as AbortSignal;
    seen.addEventListener("abort", () => reject(seen?.reason));
  }));
  const controller = new AbortController();
  const pending = api.testSavedProviderConnection(3, () => {}, undefined, controller.signal);
  await Promise.resolve();
  assert.ok(seen === controller.signal, "fetch got the caller's signal");
  controller.abort();
  await assert.rejects(pending, (error: unknown) => {
    assert.equal((error as Error).name, "AbortError");
    assert.ok(!(error instanceof ConnectionTestInterrupted));
    return true;
  });
});

test("aborting while the stream is being read ends the read with the abort", async () => {
  const controller = new AbortController();
  serve((_route, init) => {
    const signal = init?.signal as AbortSignal;
    // A body that stays open until the request is aborted, as a real response does.
    const body = new ReadableStream<Uint8Array>({
      start(stream) {
        stream.enqueue(encoder.encode('event: check\ndata: {"seq":1,"name":"dns_resolve","status":"ok","duration_ms":3}\n\n'));
        signal.addEventListener("abort", () => stream.error(signal.reason));
      },
    });
    return new Response(body, { status: 200, headers: { "Content-Type": "text/event-stream" } });
  });
  const checks: Check[] = [];
  const pending = api.testProviderConnection({ kind: "runpod" }, (c) => { checks.push(c); controller.abort(); }, controller.signal);
  await assert.rejects(pending, (error: unknown) => (error as Error).name === "AbortError");
  assert.deepEqual(checks.map((c) => c.name), ["dns_resolve"]);
});

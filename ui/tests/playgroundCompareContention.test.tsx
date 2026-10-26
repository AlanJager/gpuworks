import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { chatCatalog, controllableSSE, dom, originalFetch, renderChat, serveStreams, testStore } from "./helpers/playgroundChat.tsx";

const { act, cleanup, screen, waitFor } = await import("@testing-library/react");
const title = "Multiple columns use the same model";
const parameters = { enabled: {}, values: {}, systemPrompt: "", extraJSON: "" };

afterEach(() => {
  cleanup();
  dom.window.sessionStorage.clear();
  dom.window.localStorage.clear();
  globalThis.fetch = originalFetch;
});

test("comparison warns about shared-model contention and removes the warning after models differ", async () => {
  serveStreams([]);
  const { store } = testStore();
  const base = chatCatalog().models[0];
  const other = { ...base, id: "other-model", label: "other-model" };
  await renderChat(chatCatalog({ models: [base, other] }), { store });
  assert.equal(screen.queryByText(title), null);
  act(() => store.setMode("compare"));
  assert.ok(screen.getByText(title));
  assert.ok(screen.getByText(/Client TTFT includes network, queueing, and prefill/));
  act(() => store.setModel(store.current()!.columns[1].id, "other-model"));
  assert.equal(screen.queryByText(title), null);
  // A third column inherits the last model, so non-adjacent column pairs
  // and more than two columns must use the same duplicate check.
  act(() => store.addColumn());
  assert.ok(screen.getByText(title));
  act(() => store.setModel(store.current()!.columns[2].id, "chat-model"));
  assert.ok(screen.getByText(title));
  act(() => store.setMode("single"));
  assert.equal(screen.queryByText(title), null);
});

test("duplicate-model requests start together and keep observed waiting in each request's TTFT", async () => {
  let now = 0;
  const first = controllableSSE();
  const second = controllableSSE();
  const bodies = serveStreams([first, second]);
  const { store } = testStore({ clock: () => now });
  await store.start(1);
  store.ensureConversation({ model: "chat-model", parameters });
  store.setMode("compare");
  assert.equal(store.send("compare", [], []).status, "sent");
  // Neither stream has delivered any headers/body data or ended. A serial
  // dispatcher would only have reached fetch for the first column here.
  assert.equal(bodies.length, 2);
  assert.deepEqual(bodies.map((body) => body.model), ["chat-model", "chat-model"]);
  assert.equal(store.getState().running, 2);
  const reply = (column: number) => store.current()!.columns[column].messages.at(-1)!;
  now = 560;
  first.push("first response");
  await waitFor(() => assert.equal(reply(0).content, "first response"));
  now = 20300;
  first.end();
  await waitFor(() => assert.equal(reply(0).streaming, false));
  assert.equal(reply(1).streaming, true);
  assert.equal(reply(1).metrics?.ttftMS, undefined);
  now = 20860;
  // Reasoning is generated output too; no fabricated queue-time deduction.
  second.frame({ choices: [{ delta: { reasoning_content: "delayed thought" } }] });
  await waitFor(() => assert.equal(reply(1).reasoning, "delayed thought"));
  second.end();
  await waitFor(() => assert.equal(reply(1).streaming, false));
  assert.equal(reply(0).metrics?.ttftMS, 560);
  assert.equal(reply(1).metrics?.ttftMS, 20860);
});

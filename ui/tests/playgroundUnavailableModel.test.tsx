import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { chatCatalog, chatTree, controllableSSE, dom, locale, originalFetch, renderChat, serveStreams, testStore } from "./helpers/playgroundChat.tsx";
import { openConversationDB } from "../src/pages/playground/conversations/db.ts";
import { newColumn, newConversation } from "../src/pages/playground/conversations/model.ts";

const { cleanup, fireEvent, screen, waitFor } = await import("@testing-library/react");
const chat = locale.playground.chat;
const pane = locale.playground.pane;
const unavailable = "Model unavailable: chat-model";
const instruction = "Select an available model to continue. Your conversation is kept.";
const base = chatCatalog().models[0];
const other = { ...base, id: "other-model", label: "other-model" };

afterEach(() => {
  cleanup();
  dom.window.sessionStorage.clear();
  dom.window.localStorage.clear();
  globalThis.fetch = originalFetch;
});

async function chooseOther() {
  const selector = screen.getByRole("combobox", { name: pane.model }).closest(".ant-select")!;
  fireEvent.mouseDown(selector.querySelector(".ant-select-selector")!);
  await waitFor(() => assert.ok(document.querySelector('[title="other-model"]')));
  fireEvent.click(document.querySelector('.ant-select-dropdown [title="other-model"]')!);
}

test("catalog removal marks the selected model unavailable, preserves input, and recovers after selection", async () => {
  const stream = controllableSSE();
  const bodies = serveStreams([stream]);
  const { store } = testStore();
  const user = await renderChat(chatCatalog({ models: [base, other] }), { store });
  const input = screen.getByRole("textbox", { name: chat.inputAria });
  await user.type(input, "keep this question");
  user.view.rerender(chatTree(chatCatalog({ models: [other] }), store));
  assert.ok(screen.queryByText(unavailable), "deleted model must be visibly unavailable");
  assert.ok(screen.getByText(instruction));
  assert.match(screen.getByRole("combobox", { name: pane.model }).closest(".ant-select")!.textContent!, /unavailable/i);
  assert.equal((screen.getByRole("button", { name: new RegExp(chat.send) }) as HTMLButtonElement).disabled, true);
  fireEvent.keyDown(input, { key: "Enter" });
  assert.equal(bodies.length, 0);
  assert.equal((input as HTMLTextAreaElement).value, "keep this question");
  assert.equal(store.current()!.columns[0].model, "chat-model");
  await chooseOther();
  assert.equal(screen.queryByText(unavailable), null);
  fireEvent.keyDown(input, { key: "Enter" });
  await waitFor(() => assert.equal(bodies.length, 1));
  assert.equal(bodies[0].model, "other-model");
  stream.end();
});

test("a stale bookmark is explained even when no models are available", async () => {
  const bodies = serveStreams([]);
  const { store } = testStore();
  const user = await renderChat(chatCatalog({ models: [] }), { store, path: "/playground/chat?model=chat-model" });
  assert.ok(screen.queryByText(unavailable), "deleted model must be visibly unavailable");
  assert.ok(screen.getByText(pane.noModels));
  await user.type(screen.getByRole("textbox", { name: chat.inputAria }), "draft{Enter}");
  assert.equal((screen.getByRole("button", { name: new RegExp(chat.send) }) as HTMLButtonElement).disabled, true);
  assert.equal(bodies.length, 0);
  assert.equal(store.current()!.columns[0].model, "chat-model");
});

test("a persisted conversation keeps its history and deleted model until the user reselects", async () => {
  serveStreams([]);
  const { store, idb } = testStore();
  const conversation = newConversation(1, "single", [newColumn("chat-model", { enabled: {}, values: {}, systemPrompt: "", extraJSON: "" })], Date.now());
  conversation.columns[0].messages = [{ id: "question", role: "user", content: "saved question" }, { id: "reply", role: "assistant", content: "saved answer", model: "chat-model" }];
  await (await openConversationDB(idb)).put(conversation);
  await renderChat(chatCatalog({ models: [other] }), { store });
  assert.ok(screen.queryByText(unavailable), "deleted model must be visibly unavailable");
  assert.ok(screen.getByText("saved question"));
  assert.ok(screen.getByText("saved answer"));
  assert.equal(store.current()!.columns[0].model, "chat-model");
  await chooseOther();
  assert.ok(screen.getByText("saved answer"));
  assert.equal(store.current()!.columns[0].model, "other-model");
});


test("a model present without chat is identified as unsupported rather than deleted", async () => {
  const bodies = serveStreams([]);
  const completion = { ...base, operations: [{ type: "openai.completions" as const, endpoint: "/v1/completions", renderer: "completion" as const, response: "sse" as const, mode: "stream" as const, request_encodings: ["json" as const] }] };
  const { store } = testStore();
  const user = await renderChat(chatCatalog({ models: [completion, other] }), { store, path: "/playground/chat?model=chat-model" });
  assert.ok(screen.getByText(locale.playground.page.unsupportedModel.replace("{{model}}", "chat-model")));
  assert.equal(screen.queryByText(unavailable), null);
  assert.ok(screen.getByRole("link", { name: "openai.completions" }));
  await user.type(screen.getByRole("textbox", { name: chat.inputAria }), "question{Enter}");
  assert.equal((screen.getByRole("button", { name: new RegExp(chat.send) }) as HTMLButtonElement).disabled, true);
  assert.equal(bodies.length, 0);
});

test("comparison sends nothing when one of its models is unavailable", async () => {
  const bodies = serveStreams([]);
  const { store, idb } = testStore();
  const parameters = { enabled: {}, values: {}, systemPrompt: "", extraJSON: "" };
  const conversation = newConversation(1, "compare", [newColumn("other-model", parameters), newColumn("chat-model", parameters)], Date.now());
  await (await openConversationDB(idb)).put(conversation);
  const user = await renderChat(chatCatalog({ models: [other] }), { store });
  await user.type(screen.getByRole("textbox", { name: chat.inputAria }), "compare this{Enter}");
  assert.ok(screen.getByText(unavailable));
  assert.equal((screen.getByRole("button", { name: new RegExp(chat.send) }) as HTMLButtonElement).disabled, true);
  assert.equal(bodies.length, 0);
  assert.equal(store.current()!.columns[0].messages.length, 0);
  assert.equal(store.current()!.columns[1].messages.length, 0);
});

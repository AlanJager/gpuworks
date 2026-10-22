import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { chatCatalog, controllableSSE, dom, locale, originalFetch, renderChat, sendPrompt, serveStreams, testStore } from "./helpers/playgroundChat.tsx";
import { openConversationDB } from "../src/pages/playground/conversations/db.ts";
import { newColumn, newConversation } from "../src/pages/playground/conversations/model.ts";
import { streamLimit } from "../src/pages/playground/streamLimit.ts";

const { cleanup, fireEvent, screen, waitFor, within } = await import("@testing-library/react");
const chat = locale.playground.chat as Record<string, string>;
const pane = locale.playground.pane as Record<string, string>;
const list = locale.playground.conversations as Record<string, string>;
const fill = (text: string, values: Record<string, string | number>) => Object.entries(values).reduce((out, [key, value]) => out.replace(`{{${key}}}`, String(value)), text);

afterEach(() => {
  cleanup();
  dom.window.sessionStorage.clear();
  dom.window.localStorage.clear();
  globalThis.fetch = originalFetch;
});

const base = chatCatalog().models[0];
const twoModels = chatCatalog({ models: [base, { ...base, id: "other-model", label: "other-model" }] });
const parameters = { enabled: {}, values: {}, systemPrompt: "", extraJSON: "" };

const sidebar = () => screen.getByRole("complementary", { name: list.list });
const grid = () => within(document.querySelector<HTMLElement>(".playground-chat-grid")!);
const copyButtons = () => screen.queryAllByRole("button", { name: pane.copyReply });

async function chooseModel(label: string, column: string) {
  const combobox = screen.getByRole("combobox", { name: column });
  fireEvent.mouseDown(combobox.closest(".ant-select")!.querySelector(".ant-select-selector") as HTMLElement);
  let option: Element | undefined;
  await waitFor(() => {
    option = [...document.querySelectorAll(".ant-select-dropdown .ant-select-item-option")].find((element) => element.textContent === label);
    assert.ok(option, `option ${label}`);
  });
  fireEvent.click(option!);
}

test("leaving the chat page and coming back finds the reply still streaming", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const { store } = testStore();
  const first = await renderChat(undefined, { store });
  await sendPrompt(first, "tell me");
  stream.push("first part");
  await screen.findByText("first part");
  first.view.unmount();

  stream.push(" and more");
  await waitFor(() => assert.equal(store.current()!.columns[0].messages[1].content, "first part and more"));
  await renderChat(undefined, { store });
  await screen.findByText("first part and more");
  assert.ok(screen.getByRole("button", { name: new RegExp(pane.stop) }), "still generating");
  stream.end();
  await waitFor(() => assert.equal(copyButtons().length, 1));
});

test("the list opens, renames and deletes conversations", async () => {
  const streams = [controllableSSE(), controllableSSE()];
  serveStreams(streams);
  const user = await renderChat();
  await sendPrompt(user, "first question");
  streams[0].push("first answer");
  streams[0].end();
  await screen.findByText("first answer");
  await user.click(within(sidebar()).getByRole("button", { name: new RegExp(list.new) }));
  assert.ok(grid().queryByText("first answer") === null, "a new conversation starts empty");
  await sendPrompt(user, "second question");
  streams[1].end();
  await waitFor(() => assert.equal(copyButtons().length, 0));

  const today = within(sidebar()).getByRole("region", { name: list.today });
  await user.click(within(today).getByRole("button", { name: /^first question/ }));
  await screen.findByText("first answer");

  await user.click(within(sidebar()).getByRole("button", { name: fill(list.more, { title: "first question" }) }));
  await user.click(await screen.findByRole("menuitem", { name: new RegExp(list.rename) }));
  const title = within(sidebar()).getByRole("textbox", { name: list.titleAria });
  await user.clear(title);
  await user.type(title, "Renamed{Enter}");
  await within(sidebar()).findByRole("button", { name: /^Renamed/ });

  await user.click(within(sidebar()).getByRole("button", { name: fill(list.more, { title: "second question" }) }));
  await user.click(await screen.findByRole("menuitem", { name: new RegExp(list.delete) }));
  const dialog = await screen.findByRole("dialog", { name: list.deleteTitle });
  await user.click(within(dialog).getByRole("button", { name: list.delete }));
  await waitFor(() => assert.ok(within(sidebar()).queryByRole("button", { name: /^second question/ }) === null));
  assert.ok(within(sidebar()).getByRole("button", { name: /^Renamed/ }));
});

test("a conversation left generating shows it in the list, also when the list is collapsed", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const user = await renderChat();
  await sendPrompt(user, "slow one");
  await user.click(within(sidebar()).getByRole("button", { name: new RegExp(list.new) }));
  assert.ok(within(sidebar()).getByLabelText(list.generating));
  await user.click(within(sidebar()).getByRole("button", { name: list.collapse }));
  assert.ok(within(sidebar()).getByTitle(fill(list.generatingCount, { count: 1 })));
  await user.click(within(sidebar()).getByRole("button", { name: list.expand }));
  stream.end();
  await waitFor(() => assert.ok(within(sidebar()).queryByLabelText(list.generating) === null));
});

test("over HTTP/1.1 a send beyond four replies is refused and explained", async () => {
  const streams = Array.from({ length: 3 }, () => controllableSSE());
  const bodies = serveStreams(streams);
  const { store } = testStore({ limit: () => streamLimit("http/1.1") });
  const user = await renderChat(chatCatalog(), { store });
  await user.click(screen.getByText(chat.compare));
  await user.click(screen.getByRole("button", { name: chat.addColumn }));
  await sendPrompt(user, "three");
  await screen.findByText(fill(chat.generatingCount, { running: 3, limit: 4 }));
  await user.click(within(sidebar()).getByRole("button", { name: new RegExp(list.new) }));
  await sendPrompt(user, "three more");
  const alert = await screen.findByRole("alert");
  assert.match(alert.textContent ?? "", new RegExp(fill(chat.limitTitle, { limit: 4 }).replace(/[()]/g, "\\$&")));
  assert.match(alert.textContent ?? "", new RegExp(fill(chat.limitHttp1, { limit: 4, multiplexed: 12 }).replace(/[()]/g, "\\$&")));
  assert.equal(bodies.length, 3);
  assert.equal((screen.getByRole("textbox", { name: chat.inputAria }) as HTMLTextAreaElement).value, "three more", "the question stays for a retry");
  streams.forEach((stream) => stream.end());
});

test("over HTTP/2 the refusal only asks to wait", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const { store } = testStore({ limit: () => ({ ...streamLimit("h2"), limit: 1 }) });
  const user = await renderChat(chatCatalog(), { store });
  await sendPrompt(user, "one");
  await user.click(within(sidebar()).getByRole("button", { name: new RegExp(list.new) }));
  await sendPrompt(user, "two");
  const alert = await screen.findByRole("alert");
  assert.match(alert.textContent ?? "", new RegExp(chat.limitWait));
  stream.end();
});

test("switching mode on a conversation with messages asks, then opens a new one", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const user = await renderChat();
  await sendPrompt(user, "single question");
  stream.end();
  await waitFor(() => assert.equal(copyButtons().length, 0));

  await user.click(screen.getByText(chat.compare));
  await screen.findByText(chat.toCompareConfirm);
  await user.click(screen.getByRole("button", { name: list.cancel }));
  assert.equal(screen.getAllByRole("combobox").length, 1, "still single");

  await user.click(screen.getByText(chat.compare));
  await user.click(await screen.findByRole("button", { name: chat.newConversation }));
  await waitFor(() => assert.equal(screen.getAllByRole("combobox").length, 2));
  assert.ok(grid().queryByText("single question") === null);
  assert.ok(within(sidebar()).getByRole("button", { name: /^single question/ }), "the old one stays in the list");
});

test("removing a compare column with history asks first", async () => {
  const streams = Array.from({ length: 3 }, () => controllableSSE());
  serveStreams(streams);
  const user = await renderChat();
  await user.click(screen.getByText(chat.compare));
  await user.click(screen.getByRole("button", { name: chat.addColumn }));
  await sendPrompt(user, "three ways");
  streams.forEach((stream) => stream.end());
  await user.click(screen.getByRole("button", { name: chat.removeColumn }));
  await screen.findByText(fill(chat.removeColumnConfirm, { number: 3 }));
  const confirm = screen.getAllByRole("button", { name: chat.removeColumn }).find((button) => button.closest(".ant-popover"));
  await user.click(confirm!);
  await waitFor(() => assert.equal(screen.getAllByRole("combobox").length, 2));
});

test("switching the model keeps the conversation and marks where the new model took over", async () => {
  const streams = [controllableSSE(), controllableSSE()];
  const bodies = serveStreams(streams);
  const user = await renderChat(twoModels);
  await sendPrompt(user, "one");
  streams[0].push("answer from the first");
  streams[0].end();
  await screen.findByText("answer from the first");
  await chooseModel("other-model", pane.model);
  const marker = fill(pane.modelSwitched, { model: "other-model" });
  await screen.findByText(marker);
  await sendPrompt(user, "two");
  streams[1].push("answer from the other");
  streams[1].end();
  await screen.findByText("answer from the other");
  assert.equal(screen.getAllByText(marker).length, 1, "one marker, before the question the new model answered");
  const names = [...document.querySelectorAll(".playground-message-meta strong")].map((element) => element.textContent);
  assert.deepEqual(names, ["chat-model", "other-model"]);
  assert.equal(bodies[1].model, "other-model");
  assert.equal((bodies[1].messages as unknown[]).length, 3);
});

test("a deployment link opens its model in the empty conversation, or in a new one", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const { store } = testStore();
  const first = await renderChat(twoModels, { store, path: "/playground/chat?model=other-model" });
  assert.equal(screen.getByRole("combobox", { name: pane.model }).closest(".ant-select")!.textContent, "other-model");
  await sendPrompt(first, "on other");
  stream.end();
  await waitFor(() => assert.equal(store.current()!.columns[0].messages[1].streaming, false));
  first.view.unmount();

  await renderChat(twoModels, { store, path: "/playground/chat?model=chat-model" });
  await waitFor(() => assert.equal(store.getState().conversations.length, 2));
  assert.equal(store.current()!.columns[0].model, "chat-model");
  assert.ok(grid().queryByText("on other") === null);
});

test("a reply cut off by a reload is shown as interrupted", async () => {
  const { store, idb } = testStore();
  const conversation = newConversation(1, "single", [newColumn("chat-model", parameters)], Date.now());
  conversation.title = "cut off";
  conversation.columns[0].messages = [{ id: "u", role: "user", content: "go" }, { id: "a", role: "assistant", content: "half", model: "chat-model", streaming: true, owner: "page-gone" }];
  await (await openConversationDB(idb)).put(conversation);
  serveStreams([]);
  await renderChat(chatCatalog(), { store });
  await screen.findByText(pane.interrupted);
  assert.ok(screen.getByText("half"));
});

test("without browser storage the list says conversations are not kept", async () => {
  serveStreams([]);
  const { store } = testStore({ openDB: () => Promise.reject(new Error("blocked")) });
  await renderChat(chatCatalog(), { store });
  assert.ok(within(sidebar()).getByText(list.notStored));
});

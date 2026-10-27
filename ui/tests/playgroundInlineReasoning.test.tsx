import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { controllableSSE, dom, locale, originalFetch, renderChat, sendPrompt, serveStreams, testStore } from "./helpers/playgroundChat.tsx";
import { openConversationDB } from "../src/pages/playground/conversations/db.ts";
import { newColumn, newConversation } from "../src/pages/playground/conversations/model.ts";

const { cleanup, screen, waitFor } = await import("@testing-library/react");
const reasoningName = new RegExp(locale.playground.pane.reasoning);

afterEach(() => {
  cleanup();
  dom.window.sessionStorage.clear();
  dom.window.localStorage.clear();
  globalThis.fetch = originalFetch;
  delete (navigator as { clipboard?: unknown }).clipboard;
});

test("inline reasoning streams separately across split tags and only the answer is copied or sent as history", async () => {
  const stream = controllableSSE();
  const followup = controllableSSE();
  const bodies = serveStreams([stream, followup]);
  const { store } = testStore();
  const user = await renderChat(undefined, { store });
  const copied: string[] = [];
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async (text: string) => { copied.push(text); } } });
  await sendPrompt(user, "Please reason");
  stream.push("<thi");
  await waitFor(() => assert.equal(store.current()!.columns[0].messages.at(-1)!.content, "<thi"));
  assert.ok(document.querySelector(".is-assistant .markdown-content") === null, "a partial opening tag must not become answer text");
  stream.push("nk>Private thought");
  const toggle = await screen.findByRole("button", { name: reasoningName });
  assert.ok(screen.getByText("Private thought"));
  toggle.focus();
  await user.keyboard("{Enter}");
  assert.equal(toggle.getAttribute("aria-expanded"), "false");
  stream.push("</thi");
  stream.push("nk>Public answer");
  await screen.findByText("Public answer");
  assert.equal(toggle.getAttribute("aria-expanded"), "false");
  stream.end();
  await user.click(await screen.findByRole("button", { name: locale.playground.pane.copyReply }));
  await waitFor(() => assert.deepEqual(copied, ["Public answer"]));
  await sendPrompt(user, "Continue");
  assert.deepEqual((bodies[1].messages as { role: string; content: string }[]).find((message) => message.role === "assistant"), { role: "assistant", content: "Public answer" });
  followup.push("Done");
  followup.end();
  await screen.findByText("Done");
});

test("restored inline and closing-only reasoning stays separate in comparison columns", async () => {
  serveStreams([]);
  const { store, idb } = testStore();
  const parameters = { enabled: {}, values: {}, systemPrompt: "", extraJSON: "" };
  const conversation = newConversation(1, "compare", [newColumn("chat-model", parameters), newColumn("chat-model", parameters)], Date.now());
  conversation.columns[0].messages = [{ id: "a", role: "assistant", content: "<think>Saved thought</think>First answer", model: "chat-model" }];
  conversation.columns[1].messages = [{ id: "b", role: "assistant", content: "Template-prefilled thought</think>Second answer", model: "chat-model" }];
  await (await openConversationDB(idb)).put(conversation);
  await renderChat(undefined, { store });
  const toggles = screen.getAllByRole("button", { name: reasoningName });
  assert.equal(toggles.length, 2);
  for (const text of ["Saved thought", "Template-prefilled thought"]) assert.ok(screen.getByText(text).closest(".ant-collapse"));
  for (const text of ["First answer", "Second answer"]) assert.ok(screen.getByText(text).closest(".ant-collapse") === null);
});

test("stopping an unfinished inline thought preserves it in reasoning without inventing an answer", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const user = await renderChat();
  await sendPrompt(user, "Think slowly");
  stream.push("<think>Unfinished thought");
  await screen.findByRole("button", { name: reasoningName });
  await user.click(screen.getByRole("button", { name: new RegExp(locale.playground.pane.stop) }));
  await screen.findByText(locale.playground.pane.generationStopped);
  assert.ok(screen.getByText("Unfinished thought").closest(".ant-collapse"));
  assert.ok(screen.queryByRole("button", { name: locale.playground.pane.copyReply }) === null);
});

test("indented and quoted code delimiters survive copying and subsequent conversation history", async () => {
  const stream = controllableSSE();
  const followup = controllableSSE();
  const bodies = serveStreams([stream, followup]);
  const user = await renderChat();
  const content = "Literal examples:\n\n    </think>\n\n> ~~~xml\n> </think>\n> ~~~\n\nKeep this answer.";
  const copied: string[] = [];
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async (text: string) => { copied.push(text); } } });
  await sendPrompt(user, "Explain the delimiters");
  stream.push(content);
  stream.end();
  const copy = await screen.findByRole("button", { name: locale.playground.pane.copyReply });
  assert.ok(screen.queryByRole("button", { name: reasoningName }) === null);
  assert.ok(screen.getByText("Literal examples:"));
  assert.equal(screen.getAllByText("</think>").length, 2);
  await user.click(copy);
  await waitFor(() => assert.deepEqual(copied, [content]));
  await sendPrompt(user, "Continue");
  assert.deepEqual((bodies[1].messages as { role: string; content: string }[]).find((message) => message.role === "assistant"), { role: "assistant", content });
  followup.push("Done");
  followup.end();
  await screen.findByText("Done");
});

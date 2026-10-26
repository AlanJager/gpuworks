import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { controllableSSE, dom, locale, originalFetch, renderChat, sendPrompt, serveStreams, testStore } from "./helpers/playgroundChat.tsx";
import { openConversationDB } from "../src/pages/playground/conversations/db.ts";
import { newColumn, newConversation } from "../src/pages/playground/conversations/model.ts";

const { cleanup, screen, waitFor } = await import("@testing-library/react");

afterEach(() => {
  cleanup();
  dom.window.sessionStorage.clear();
  dom.window.localStorage.clear();
  globalThis.fetch = originalFetch;
});

test("reasoning appears expanded before the answer and respects keyboard collapse during streaming", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const user = await renderChat();
  await sendPrompt(user, "think about this");
  stream.frame({ choices: [{ delta: { reasoning_content: "First thought" } }] });
  const toggle = await screen.findByRole("button", { name: new RegExp(locale.playground.pane.reasoning) });
  assert.equal(toggle.getAttribute("aria-expanded"), "true");
  assert.ok(screen.getByText("First thought"));
  assert.equal(screen.queryByRole("button", { name: locale.playground.pane.copyReply }), null);
  stream.frame({ choices: [{ delta: { reasoning_content: "; next thought" } }] });
  await screen.findByText("First thought; next thought");
  toggle.focus();
  await user.keyboard("{Enter}");
  assert.equal(toggle.getAttribute("aria-expanded"), "false");
  stream.frame({ choices: [{ delta: { reasoning_content: "; final thought" } }] });
  stream.push("The answer");
  await screen.findByText("The answer");
  assert.equal(toggle.getAttribute("aria-expanded"), "false");
  stream.end();
  await screen.findByRole("button", { name: locale.playground.pane.copyReply });
  assert.equal(toggle.getAttribute("aria-expanded"), "false");
  toggle.focus();
  await user.keyboard("{Enter}");
  await waitFor(() => assert.equal(toggle.getAttribute("aria-expanded"), "true"));
  assert.ok(screen.getByText("First thought; next thought; final thought"));
});

test("a restored completed reply shows its reasoning expanded by default", async () => {
  serveStreams([]);
  const { store, idb } = testStore();
  const conversation = newConversation(1, "single", [newColumn("chat-model", { enabled: {}, values: {}, systemPrompt: "", extraJSON: "" })], Date.now());
  conversation.columns[0].messages = [
    { id: "question", role: "user", content: "saved question" },
    { id: "reply", role: "assistant", content: "saved answer", reasoning: "saved reasoning", model: "chat-model" },
  ];
  await (await openConversationDB(idb)).put(conversation);
  await renderChat(undefined, { store });
  const toggle = screen.getByRole("button", { name: new RegExp(locale.playground.pane.reasoning) });
  assert.equal(toggle.getAttribute("aria-expanded"), "true");
  assert.ok(screen.getByText("saved reasoning"));
  assert.ok(screen.getByText("saved answer"));
});

import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { chatCatalog, controllableSSE, dom, locale, originalFetch, renderChat as renderChatWith, sendPrompt, serveStreams as serve } from "./helpers/playgroundChat.tsx";

const { cleanup, fireEvent, screen, waitFor } = await import("@testing-library/react");
const pane = locale.playground.pane as Record<string, string>;
const chat = locale.playground.chat as Record<string, string>;
const renderChat = () => renderChatWith(chatCatalog());
const serveStreams = (...streams: ReturnType<typeof controllableSSE>[]) => serve(streams);

afterEach(() => {
  cleanup();
  dom.window.sessionStorage.clear();
  globalThis.fetch = originalFetch;
  delete (navigator as { clipboard?: unknown }).clipboard;
});

test("a finished reply can be copied whole as its Markdown source", async () => {
  const stream = controllableSSE();
  serveStreams(stream);
  const user = await renderChat();
  const copied: string[] = [];
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async (text: string) => { copied.push(text); } } });
  await sendPrompt(user, "list files");
  stream.push("Run:\n\n```bash\nls -la\n```\n");
  await screen.findByText("ls -la");
  assert.ok(screen.queryByRole("button", { name: pane.copyReply }) === null, "no whole-reply copy while generating");
  stream.end();
  const copyReply = await screen.findByRole("button", { name: pane.copyReply });
  await user.click(copyReply);
  await waitFor(() => assert.deepEqual(copied, ["Run:\n\n```bash\nls -la\n```\n"]));
});

// jsdom has no layout. Give each message list a viewport of 400px whose
// content height grows with its text, the way a streamed reply grows.
const VIEWPORT = 400;
function layoutLists() {
  const lists = [...document.querySelectorAll<HTMLElement>(".playground-message-list")];
  for (const list of lists) {
    Object.defineProperty(list, "clientHeight", { configurable: true, get: () => VIEWPORT });
    Object.defineProperty(list, "scrollHeight", { configurable: true, get: () => VIEWPORT + (list.textContent?.length ?? 0) * 20 });
  }
  return lists;
}
const atBottom = (list: HTMLElement) => list.scrollTop === list.scrollHeight;
const followBox = () => screen.getByRole("checkbox", { name: chat.followLatest }) as HTMLInputElement;

// userScrollUp is what a wheel turn up looks like to the page: the input
// event, then the browser moving the viewport and firing scroll.
function userScrollUp(list: HTMLElement, by = 200) {
  fireEvent.wheel(list, { deltaY: -by });
  list.scrollTop = list.scrollHeight - list.clientHeight - by;
  fireEvent.scroll(list);
}

async function streamingReply() {
  const stream = controllableSSE();
  serveStreams(stream);
  const user = await renderChat();
  const [list] = layoutLists();
  await sendPrompt(user, "write a long story");
  stream.push("first paragraph of a long reply");
  await screen.findByText("first paragraph of a long reply");
  return { stream, user, list };
}

test("a streamed reply keeps the view at the newest text while following", async () => {
  const { stream, list } = await streamingReply();
  assert.equal(followBox().checked, true);
  await waitFor(() => assert.ok(atBottom(list), "pinned to the bottom"));
  stream.push(" and a second paragraph");
  await screen.findByText(/second paragraph/);
  await waitFor(() => assert.ok(atBottom(list), "still pinned after more text"));
  stream.end();
});

test("scrolling up stops following and later text no longer moves the view", async () => {
  const { stream, list } = await streamingReply();
  userScrollUp(list);
  await waitFor(() => assert.equal(followBox().checked, false));
  const reading = list.scrollTop;
  stream.push(" more text arriving while the user reads");
  await screen.findByText(/while the user reads/);
  assert.equal(list.scrollTop, reading);
  stream.end();
});

test("scroll events without user input never stop following", async () => {
  const { stream, list } = await streamingReply();
  // A layout shift or the browser clamping scrollTop: no wheel, touch or key.
  list.scrollTop = 0;
  fireEvent.scroll(list);
  // Wheeling down is not scrolling away either.
  fireEvent.wheel(list, { deltaY: 120 });
  fireEvent.scroll(list);
  assert.equal(followBox().checked, true);
  stream.push(" next");
  await waitFor(() => assert.ok(atBottom(list)));
  stream.end();
});

test("a press on a message is not a scrollbar drag, a press on the list is", async () => {
  const { stream, list } = await streamingReply();
  fireEvent.pointerDown(screen.getByText("first paragraph of a long reply"));
  list.scrollTop = 0;
  fireEvent.scroll(list);
  assert.equal(followBox().checked, true, "selecting text is not scrolling");
  fireEvent.pointerUp(window);
  fireEvent.pointerDown(list);
  list.scrollTop = 0;
  fireEvent.scroll(list);
  await waitFor(() => assert.equal(followBox().checked, false));
  fireEvent.pointerUp(window);
  stream.end();
});

test("a text selection inside the list pauses the jump without unchecking", async () => {
  const { stream, list } = await streamingReply();
  list.scrollTop = 100;
  document.getSelection()!.selectAllChildren(screen.getByText("first paragraph of a long reply"));
  stream.push(" grows under the selection");
  await screen.findByText(/grows under the selection/);
  assert.equal(list.scrollTop, 100);
  assert.equal(followBox().checked, true);
  document.getSelection()!.removeAllRanges();
  stream.end();
});

test("checking the box again jumps to the newest text", async () => {
  const { stream, user, list } = await streamingReply();
  userScrollUp(list);
  await waitFor(() => assert.equal(followBox().checked, false));
  stream.push(" unseen tail");
  await screen.findByText(/unseen tail/);
  assert.ok(!atBottom(list));
  await user.click(followBox());
  assert.equal(followBox().checked, true);
  await waitFor(() => assert.ok(atBottom(list)));
  stream.end();
});

test("the follow checkbox toggles from the keyboard", async () => {
  const user = await renderChat();
  followBox().focus();
  await user.keyboard(" ");
  assert.equal(followBox().checked, false);
  await user.keyboard(" ");
  assert.equal(followBox().checked, true);
});

test("sending a new message follows again", async () => {
  const first = controllableSSE();
  const second = controllableSSE();
  serveStreams(first, second);
  const user = await renderChat();
  const [list] = layoutLists();
  await sendPrompt(user, "one");
  first.push("first answer");
  await screen.findByText("first answer");
  first.end();
  userScrollUp(list);
  await waitFor(() => assert.equal(followBox().checked, false));
  await sendPrompt(user, "two");
  assert.equal(followBox().checked, true);
  second.push("second answer");
  await screen.findByText("second answer");
  await waitFor(() => assert.ok(atBottom(list)));
  second.end();
});

test("in compare mode scrolling up in one column stops every column", async () => {
  const left = controllableSSE();
  const right = controllableSSE();
  serveStreams(left, right);
  const user = await renderChat();
  await user.click(screen.getByText(chat.compare));
  const [leftList, rightList] = layoutLists();
  assert.ok(leftList && rightList, "two columns");
  await user.type(screen.getByRole("textbox", { name: chat.inputAria }), "compare");
  await user.click(screen.getByRole("button", { name: new RegExp(chat.send) }));
  left.push("left answer");
  right.push("right answer");
  await screen.findByText("right answer");
  await waitFor(() => assert.ok(atBottom(rightList)));
  userScrollUp(leftList);
  await waitFor(() => assert.equal(followBox().checked, false));
  const rightReading = rightList.scrollTop;
  right.push(" keeps streaming");
  await screen.findByText(/keeps streaming/);
  assert.equal(rightList.scrollTop, rightReading);
  left.end();
  right.end();
});

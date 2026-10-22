import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { chatCatalog, controllableSSE, dom, locale, originalFetch, renderChat, sendPrompt, serveStreams } from "./helpers/playgroundChat.tsx";

const { act, cleanup, fireEvent, screen, waitFor, within } = await import("@testing-library/react");
const { PREVIEW_SANDBOX, RESTRICTED_CSP, PREVIEW_MESSAGE_SOURCE } = await import("../src/pages/playground/previewSandbox.ts");
const p = locale.playground.preview as Record<string, string>;
const chat = locale.playground.chat as Record<string, string>;

Object.defineProperty(globalThis, "DOMParser", { value: dom.window.DOMParser, configurable: true });
Object.defineProperty(globalThis, "MessageEvent", { value: dom.window.MessageEvent, configurable: true });

const opened: unknown[][] = [];
const originalOpen = dom.window.open;
dom.window.open = ((...args: unknown[]) => { opened.push(args); return null; }) as typeof dom.window.open;

afterEach(() => {
  cleanup();
  dom.window.sessionStorage.clear();
  globalThis.fetch = originalFetch;
  opened.length = 0;
});
process.on("exit", () => { dom.window.open = originalOpen; });

const page = "<!doctype html>\n<html><body><h1>Counter</h1><script>let n = 0;</script></body></html>";
const previewButtons = () => screen.queryAllByRole("button", { name: p.open });
const drawer = () => screen.getByRole("dialog", { name: p.title });
const frame = () => drawer().querySelector("iframe");

// The frame's messages arrive on the parent window with the frame as source.
function fromFrame(data: unknown, source: Window | null = frame()!.contentWindow) {
  act(() => { dom.window.dispatchEvent(new dom.window.MessageEvent("message", { data, source: source as never })); });
}

async function streamingHTMLBlock(catalog = chatCatalog(), role = "admin") {
  const stream = controllableSSE();
  serveStreams([stream], { role });
  const user = await renderChat(catalog);
  await sendPrompt(user, "make a page");
  stream.push("Here it is:\n\n```html\n<!doctype html>\n<html><body><h1>Counter</h1>");
  await waitFor(() => assert.equal(previewButtons().length, 1));
  return { stream, user };
}

async function finishedHTMLBlock(catalog = chatCatalog(), role = "admin") {
  const { stream, user } = await streamingHTMLBlock(catalog, role);
  stream.push("<script>let n = 0;</script></body></html>\n```\n\nEnjoy.");
  stream.end();
  await screen.findByText("Enjoy.");
  await user.click(previewButtons()[0]);
  await waitFor(() => assert.ok(frame(), "frame rendered"));
  return { user };
}

test("only html-like blocks in replies get a preview button", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const user = await renderChat();
  await sendPrompt(user, "two blocks");
  stream.push("```python\nprint(1)\n```\n\n```svg\n<svg></svg>\n```\n\n```HTML\n<p>x</p>\n```\n");
  stream.end();
  await screen.findByText("print(1)");
  const headers = [...document.querySelectorAll(".markdown-code-header")];
  assert.deepEqual(headers.map((header) => [header.querySelector(".markdown-code-language")?.textContent, !!within(header as HTMLElement).queryByRole("button", { name: p.open })]), [
    ["python", false], ["svg", true], ["html", true],
  ]);
});

test("a block still streaming shows its source and renders once its fence closes", async () => {
  const { stream, user } = await streamingHTMLBlock();
  await user.click(previewButtons()[0]);
  assert.ok(within(drawer()).getByText(p.generating));
  assert.equal(frame(), null, "no frame for half a document");
  assert.match(drawer().querySelector("pre.playground-preview-source")?.textContent ?? "", /<h1>Counter<\/h1>$/);
  stream.push("<p>more</p>");
  await waitFor(() => assert.match(drawer().querySelector("pre.playground-preview-source")?.textContent ?? "", /<p>more<\/p>$/));
  stream.push("</body></html>\n```\n");
  await waitFor(() => assert.ok(frame()));
  const rendered = frame()!;
  assert.equal(rendered.getAttribute("sandbox"), PREVIEW_SANDBOX);
  assert.equal(rendered.getAttribute("sandbox"), "allow-scripts allow-modals allow-downloads");
  const srcdoc = rendered.getAttribute("srcdoc") ?? "";
  assert.ok(srcdoc.includes(`content="${RESTRICTED_CSP}"`), "restrictive CSP injected");
  assert.match(srcdoc, /<p>more<\/p>/);
  assert.equal(within(drawer()).queryByText(p.generating), null);
  // Text after the block does not touch the rendered frame.
  stream.push("\n\nA closing remark.");
  await screen.findByText("A closing remark.");
  assert.ok(frame() === rendered, "the frame is not re-rendered by later text");
  stream.end();
});

test("an administrator's network switch removes the CSP but never the sandbox", async () => {
  await finishedHTMLBlock(chatCatalog({ html_preview: { allow_network: true } }));
  const rendered = frame()!;
  assert.equal(rendered.getAttribute("sandbox"), PREVIEW_SANDBOX);
  assert.doesNotMatch(rendered.getAttribute("srcdoc") ?? "", /Content-Security-Policy/);
  assert.ok(within(drawer()).getByText(p.noteOpen));
});

test("a blocked request is explained; non-admins are told to ask an administrator", async () => {
  await finishedHTMLBlock(chatCatalog(), "user");
  fromFrame({ source: PREVIEW_MESSAGE_SOURCE, type: "blocked", directive: "connect-src", uri: "https://api.example.com/v1/weather" });
  fromFrame({ source: PREVIEW_MESSAGE_SOURCE, type: "blocked", directive: "connect-src", uri: "https://api.example.com/v1/other" });
  fromFrame({ source: PREVIEW_MESSAGE_SOURCE, type: "blocked", directive: "img-src", uri: "http://10.0.0.5/logo.png" });
  const alert = await within(drawer()).findByText(p.blocked.replace("{{targets}}", "api.example.com, 10.0.0.5"));
  assert.ok(alert);
  assert.ok(within(drawer()).getByText(p.askAdmin));
  assert.equal(within(drawer()).queryByRole("link", { name: p.openSettings }), null);
});

test("an administrator gets a link to the security setting", async () => {
  const { user } = await finishedHTMLBlock();
  fromFrame({ source: PREVIEW_MESSAGE_SOURCE, type: "blocked", directive: "connect-src", uri: "wss://socket.example/feed" });
  const link = await within(drawer()).findByRole("link", { name: p.openSettings });
  assert.equal(link.getAttribute("href"), "/settings#security");
  await user.click(link);
  assert.equal((await screen.findByTestId("where")).textContent, "/settings#security");
});

test("messages that are not from the frame, or not in a known shape, are ignored", async () => {
  await finishedHTMLBlock();
  fromFrame({ source: PREVIEW_MESSAGE_SOURCE, type: "blocked", directive: "connect-src", uri: "https://spoof.example/" }, dom.window as unknown as Window);
  fromFrame({ source: PREVIEW_MESSAGE_SOURCE, type: "blocked", uri: 42 });
  fromFrame({ type: "open", url: "https://example.com/" });
  await new Promise((resolve) => setTimeout(resolve, 50));
  assert.equal(within(drawer()).queryByText(/request was blocked/), null);
  assert.equal(screen.queryByRole("dialog", { name: p.openLinkTitle }), null);
  assert.equal(opened.length, 0);
});

test("a link from the page opens in a new tab only after the user confirms it", async () => {
  const { user } = await finishedHTMLBlock();
  fromFrame({ source: PREVIEW_MESSAGE_SOURCE, type: "open", url: "javascript:alert(1)" });
  await new Promise((resolve) => setTimeout(resolve, 50));
  assert.equal(screen.queryByText(p.openLinkTitle), null, "non-http links are dropped");
  fromFrame({ source: PREVIEW_MESSAGE_SOURCE, type: "open", url: "http://intranet.example/admin" });
  const [confirm] = await screen.findAllByText(p.openLinkTitle);
  const dialog = confirm.closest(".ant-modal") as HTMLElement;
  assert.ok(within(dialog).getByText("http://intranet.example/admin"));
  assert.equal(opened.length, 0, "nothing opens before confirming");
  await user.click(within(dialog).getByRole("button", { name: p.openLink }));
  assert.deepEqual(opened, [["http://intranet.example/admin", "_blank", "noopener,noreferrer"]]);
});

test("with network allowed a link opens directly", async () => {
  await finishedHTMLBlock(chatCatalog({ html_preview: { allow_network: true } }));
  fromFrame({ source: PREVIEW_MESSAGE_SOURCE, type: "open", url: "https://example.com/next" });
  await waitFor(() => assert.deepEqual(opened, [["https://example.com/next", "_blank", "noopener,noreferrer"]]));
  assert.equal(screen.queryByText(p.openLinkTitle), null);
});

test("a page that navigates itself is put back and the user is told", async () => {
  await finishedHTMLBlock();
  const first = frame()!;
  // jsdom fires the frame's own first load; let it land.
  await new Promise((resolve) => setTimeout(resolve, 50));
  assert.equal(within(drawer()).queryByText(p.navigated), null, "the first load is not a navigation");
  fireEvent.load(first);
  assert.ok(await within(drawer()).findByText(p.navigated));
  await waitFor(() => assert.ok(frame() && frame() !== first, "a fresh frame replaces the navigated one"));
  assert.equal(frame()!.getAttribute("srcdoc"), first.getAttribute("srcdoc"));
});

test("with network allowed, navigation inside the frame is left alone", async () => {
  await finishedHTMLBlock(chatCatalog({ html_preview: { allow_network: true } }));
  const first = frame()!;
  await new Promise((resolve) => setTimeout(resolve, 50));
  fireEvent.load(first);
  await new Promise((resolve) => setTimeout(resolve, 50));
  assert.ok(frame() === first);
  assert.equal(within(drawer()).queryByText(p.navigated), null);
});

test("source view, reload and download work on the finished block", async () => {
  const { user } = await finishedHTMLBlock();
  const first = frame()!;
  await user.click(within(drawer()).getByRole("button", { name: p.reload }));
  await waitFor(() => assert.ok(frame() && frame() !== first, "reload mounts a fresh frame"));
  await user.click(within(drawer()).getByText(p.source));
  assert.equal(frame(), null);
  assert.equal(drawer().querySelector("pre.playground-preview-source")?.textContent, page);
  const blobs: Blob[] = [];
  const downloads: string[] = [];
  const createObjectURL = URL.createObjectURL;
  const click = dom.window.HTMLAnchorElement.prototype.click;
  URL.createObjectURL = ((blob: Blob) => { blobs.push(blob); return "blob:preview"; }) as typeof URL.createObjectURL;
  URL.revokeObjectURL = (() => undefined) as typeof URL.revokeObjectURL;
  dom.window.HTMLAnchorElement.prototype.click = function (this: HTMLAnchorElement) { downloads.push(`${this.download} ${this.href}`); };
  try {
    await user.click(within(drawer()).getByRole("button", { name: p.download }));
    assert.deepEqual(downloads, ["preview.html blob:preview"]);
    assert.equal(await blobs[0].text(), page);
    assert.equal(blobs[0].type, "text/html;charset=utf-8");
  } finally {
    URL.createObjectURL = createObjectURL;
    dom.window.HTMLAnchorElement.prototype.click = click;
  }
});

test("a reply that is a bare HTML document can be previewed whole", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const user = await renderChat();
  await sendPrompt(user, "page only");
  stream.push(page);
  stream.end();
  await waitFor(() => assert.equal(previewButtons().length, 1));
  await user.click(previewButtons()[0]);
  await waitFor(() => assert.ok(frame()));
  assert.match(frame()!.getAttribute("srcdoc") ?? "", /<h1>Counter<\/h1>/);
});

test("clearing the conversation closes the preview", async () => {
  const { user } = await finishedHTMLBlock();
  await user.click(screen.getByRole("button", { name: new RegExp(`${chat.clear}$`) }));
  await waitFor(() => assert.ok(screen.queryByRole("dialog", { name: p.title }) === null, "drawer gone"));
});

test("Escape closes the preview", async () => {
  await finishedHTMLBlock();
  const reload = within(drawer()).getByRole("button", { name: p.reload });
  reload.focus();
  // rc-drawer reads keyCode, which browsers set and user-event does not.
  fireEvent.keyDown(reload, { key: "Escape", code: "Escape", keyCode: 27 });
  await waitFor(() => assert.ok(screen.queryByRole("dialog", { name: p.title }) === null, "drawer gone"));
});

import assert from "node:assert/strict";
import { test } from "node:test";
import { JSDOM } from "jsdom";

const parserWindow = new JSDOM("").window;
Object.defineProperty(globalThis, "DOMParser", { value: parserWindow.DOMParser, configurable: true });

const {
  blockedTarget, buildPreviewDocument, linkToOpen, looksLikeHTMLDocument, parsePreviewMessage,
  PREVIEW_MESSAGE_SOURCE, RESTRICTED_CSP,
} = await import("../src/pages/playground/previewSandbox.ts");

const restricted = { allowNetwork: false };
const open = { allowNetwork: true };

function parse(srcdoc: string) {
  return new parserWindow.DOMParser().parseFromString(srcdoc, "text/html");
}

test("the restrictive policy puts charset, CSP and bootstrap ahead of everything the page has", () => {
  const cases = {
    document: "<!doctype html><html><head><title>Page</title><script>window.pageRan = 1</script></head><body><h1>Hi</h1></body></html>",
    fragment: "<h1>Hi</h1><script>window.pageRan = 1</script>",
    svg: "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"10\" height=\"10\"><rect width=\"10\" height=\"10\"/></svg>",
    headInComment: "<!-- <head><meta http-equiv=\"Content-Security-Policy\" content=\"default-src *\"></head> --><html><head><title>Page</title></head><body>x</body></html>",
  };
  for (const [name, source] of Object.entries(cases)) {
    const srcdoc = buildPreviewDocument(source, restricted);
    assert.ok(srcdoc.startsWith("<!doctype html>\n"), name);
    const doc = parse(srcdoc);
    const [charset, csp, script] = [...doc.head.children];
    assert.equal(charset.getAttribute("charset"), "utf-8", name);
    assert.equal(csp.getAttribute("http-equiv"), "Content-Security-Policy", name);
    assert.equal(csp.getAttribute("content"), RESTRICTED_CSP, name);
    assert.equal(script.tagName, "SCRIPT", name);
    assert.match(script.textContent ?? "", /securitypolicyviolation/, name);
    // Our CSP is the first one in the document, ahead of any the page brings.
    assert.ok(doc.querySelector("meta[http-equiv='Content-Security-Policy']") === csp, name);
  }
  const svgDoc = parse(buildPreviewDocument(cases.svg, restricted));
  assert.ok(svgDoc.body.querySelector("svg rect"), "a bare svg lands in the body");
  const page = parse(buildPreviewDocument(cases.document, restricted));
  assert.equal(page.title, "Page");
  assert.equal(page.body.querySelector("h1")?.textContent, "Hi");
});

test("a page's own permissive CSP comes after ours, so it can only narrow it", () => {
  const source = "<html><head><meta http-equiv=\"Content-Security-Policy\" content=\"default-src *; connect-src *\"></head><body></body></html>";
  const policies = [...parse(buildPreviewDocument(source, restricted)).querySelectorAll("meta[http-equiv='Content-Security-Policy']")].map((meta) => meta.getAttribute("content"));
  assert.deepEqual(policies, [RESTRICTED_CSP, "default-src *; connect-src *"]);
});

test("the restrictive CSP blocks connections and plain http, and keeps eval and wasm", () => {
  const directives = new Map(RESTRICTED_CSP.split("; ").map((part) => {
    const [name, ...values] = part.split(" ");
    return [name, values] as const;
  }));
  assert.deepEqual(directives.get("default-src"), ["'none'"]);
  assert.deepEqual(directives.get("connect-src"), ["'none'"]);
  assert.deepEqual(directives.get("form-action"), ["'none'"]);
  assert.deepEqual(directives.get("base-uri"), ["'none'"]);
  assert.deepEqual(directives.get("script-src"), ["'unsafe-inline'", "'unsafe-eval'", "'wasm-unsafe-eval'", "https:"]);
  for (const [name, values] of directives) {
    assert.ok(!values.includes("http:") && !values.includes("*"), `${name} must not allow plain http or any host`);
  }
});

test("with network allowed there is no CSP and no violation reporter, but the bootstrap stays", () => {
  const head = parse(buildPreviewDocument("<p>x</p>", open)).head;
  assert.equal(head.querySelector("meta[http-equiv='Content-Security-Policy']"), null);
  const script = head.querySelector("script");
  assert.ok(script && script === head.children[1], "bootstrap right after charset");
  assert.doesNotMatch(script.textContent ?? "", /securitypolicyviolation/);
  assert.match(script.textContent ?? "", /localStorage/);
});

// runBootstrap executes the bootstrap from a built srcdoc inside a fresh
// window, the way the frame would, and collects what it posts to the parent
// (a top-level window is its own parent).
function runBootstrap(options: { allowNetwork: boolean }, body = "", prepare?: (window: Window) => void) {
  const window = new JSDOM(`<!doctype html><body>${body}</body>`, { runScripts: "outside-only", url: "https://preview.invalid/" }).window;
  prepare?.(window as unknown as Window);
  const posted: unknown[] = [];
  // JSON round trip: the posted objects belong to the other realm.
  window.postMessage = ((data: unknown) => { posted.push(JSON.parse(JSON.stringify(data))); }) as typeof window.postMessage;
  const script = parse(buildPreviewDocument("<p>x</p>", options)).head.querySelector("script")!.textContent!;
  window.eval(script);
  return { window, posted };
}

test("http(s) link clicks go to the parent; anchors, downloads and javascript: stay in the page", () => {
  const { window, posted } = runBootstrap(restricted, `
    <a id="out" href="https://example.com/docs?a=1">out</a>
    <a id="plain" href="http://intranet.example/x">plain</a>
    <a id="anchor" href="#section">anchor</a>
    <a id="file" href="https://example.com/data.csv" download>file</a>
    <a id="js" href="javascript:void(0)">js</a>
    <a id="handled" href="https://example.com/handled">handled</a>`);
  window.document.getElementById("handled")!.addEventListener("click", (event) => event.preventDefault());
  const click = (id: string) => {
    const event = new window.MouseEvent("click", { bubbles: true, cancelable: true });
    window.document.getElementById(id)!.dispatchEvent(event);
    return event.defaultPrevented;
  };
  assert.equal(click("out"), true, "the frame does not navigate itself");
  assert.equal(click("plain"), true);
  assert.equal(click("anchor"), false);
  assert.equal(click("file"), false);
  assert.equal(click("js"), false);
  click("handled");
  assert.deepEqual(posted, [
    { source: PREVIEW_MESSAGE_SOURCE, type: "open", url: "https://example.com/docs?a=1" },
    { source: PREVIEW_MESSAGE_SOURCE, type: "open", url: "http://intranet.example/x" },
  ]);
});

test("CSP violations are reported under the restrictive policy only", () => {
  const violation = (window: JSDOM["window"]) => {
    const event = new window.Event("securitypolicyviolation");
    Object.assign(event, { effectiveDirective: "connect-src", blockedURI: "https://api.example.com/v1/data" });
    window.document.dispatchEvent(event);
  };
  const strict = runBootstrap(restricted);
  violation(strict.window);
  assert.deepEqual(strict.posted, [{ source: PREVIEW_MESSAGE_SOURCE, type: "blocked", directive: "connect-src", uri: "https://api.example.com/v1/data" }]);
  const permissive = runBootstrap(open);
  violation(permissive.window);
  assert.deepEqual(permissive.posted, []);
});

test("storage and cookies that throw in an opaque origin get in-memory stand-ins", () => {
  const { window } = runBootstrap(restricted, "", (target) => {
    const denied = () => { throw new DOMException("The document is sandboxed", "SecurityError"); };
    Object.defineProperty(target, "localStorage", { configurable: true, get: denied });
    Object.defineProperty(target, "sessionStorage", { configurable: true, get: denied });
    Object.defineProperty(target.document, "cookie", { configurable: true, get: denied, set: denied });
  });
  const storage = window.localStorage;
  storage.setItem("todo", "1");
  storage.setItem("theme", "dark");
  assert.equal(storage.getItem("todo"), "1");
  assert.equal(storage.length, 2);
  assert.equal(storage.key(1), "theme");
  storage.removeItem("todo");
  assert.equal(storage.getItem("todo"), null);
  storage.clear();
  assert.equal(storage.length, 0);
  window.sessionStorage.setItem("k", "v");
  assert.equal(window.sessionStorage.getItem("k"), "v");
  window.document.cookie = "a=1; path=/";
  window.document.cookie = "b=2";
  window.document.cookie = "a=3";
  assert.equal(window.document.cookie, "a=3; b=2");
});

test("working storage is left alone", () => {
  const { window } = runBootstrap(restricted);
  window.localStorage.setItem("real", "yes");
  assert.equal(window.localStorage.constructor.name, "Storage");
});

test("only the two known message shapes from the frame are accepted", () => {
  assert.deepEqual(parsePreviewMessage({ source: PREVIEW_MESSAGE_SOURCE, type: "open", url: "https://x.test" }), { type: "open", url: "https://x.test" });
  assert.deepEqual(parsePreviewMessage({ source: PREVIEW_MESSAGE_SOURCE, type: "blocked", directive: "img-src", uri: "http://a" }), { type: "blocked", directive: "img-src", uri: "http://a" });
  for (const bad of [null, "open", { type: "open", url: "https://x.test" }, { source: "other", type: "open", url: "u" }, { source: PREVIEW_MESSAGE_SOURCE, type: "open", url: 7 }, { source: PREVIEW_MESSAGE_SOURCE, type: "eval", code: "x" }]) {
    assert.equal(parsePreviewMessage(bad), undefined, JSON.stringify(bad));
  }
});

test("links open only over http(s); blocked targets are named by host", () => {
  assert.equal(linkToOpen("https://example.com/a"), "https://example.com/a");
  assert.equal(linkToOpen("http://intranet:8080/"), "http://intranet:8080/");
  for (const bad of ["javascript:alert(1)", "data:text/html,x", "file:///etc/passwd", "not a url"]) assert.equal(linkToOpen(bad), undefined, bad);
  assert.equal(blockedTarget("https://cdn.example.com/lib.js?v=1"), "cdn.example.com");
  assert.equal(blockedTarget("http://10.0.0.5:8800/api/v1/nodes"), "10.0.0.5:8800");
  assert.equal(blockedTarget("ws://realtime.example/socket"), "realtime.example");
  assert.equal(blockedTarget("inline"), "inline");
});

test("a reply that is itself an HTML document is recognized", () => {
  assert.equal(looksLikeHTMLDocument("<!DOCTYPE html>\n<html><body>x</body></html>"), true);
  assert.equal(looksLikeHTMLDocument("  \n<html lang=\"en\"><body>x</body></html>"), true);
  assert.equal(looksLikeHTMLDocument("<html>"), true);
  assert.equal(looksLikeHTMLDocument("Here is a page:\n```html\n<html></html>\n```"), false);
  assert.equal(looksLikeHTMLDocument("<htmlx>"), false);
});

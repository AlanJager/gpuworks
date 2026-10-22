import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createRequire } from "node:module";

// Render the actual CodeMirror editor, which the normal browser uses; the
// detail-page suite exercises its separate plain-textarea fallback.
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/", pretendToBeVisual: true });
for (const name of ["window", "document", "navigator", "Window", "HTMLElement", "Element", "Node", "Text", "Range", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: dom.window.getComputedStyle.bind(dom.window), configurable: true });
Object.defineProperty(globalThis, "requestAnimationFrame", { value: dom.window.requestAnimationFrame.bind(dom.window), configurable: true });
Object.defineProperty(globalThis, "cancelAnimationFrame", { value: dom.window.cancelAnimationFrame.bind(dom.window), configurable: true });
Object.defineProperties(dom.window.Range.prototype, {
  getClientRects: { configurable: true, value: () => [] },
  getBoundingClientRect: { configurable: true, value: () => new dom.window.DOMRect() },
});
const { cleanup, render, screen } = await import("@testing-library/react");
// The schema dependency exposes its Node-compatible entry through CommonJS.
const { default: YamlEditor } = createRequire(import.meta.url)("../src/pages/yamlEditor.tsx") as typeof import("../src/pages/yamlEditor.tsx");
afterEach(cleanup);
after(() => dom.window.close());

test("the real YAML editing surface has its localized accessible name", () => {
  const { rerender } = render(<YamlEditor value="models: []" onChange={() => {}} ariaLabel="Edit flat YAML" />);
  const editor = screen.getByRole("textbox", { name: "Edit flat YAML" });
  assert.equal(editor.getAttribute("contenteditable"), "true");
  assert.equal(editor.textContent, "models: []");
  rerender(<YamlEditor value="models: []" onChange={() => {}} ariaLabel="编辑 YAML" />);
  assert.equal(screen.getByRole("textbox", { name: "编辑 YAML" }), editor);
  assert.equal(screen.queryByRole("textbox", { name: "Edit flat YAML" }), null);
});

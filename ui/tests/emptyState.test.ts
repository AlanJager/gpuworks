import assert from "node:assert/strict";
import { JSDOM } from "jsdom";
import React from "react";
import { test, afterEach } from "node:test";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
Object.defineProperty(globalThis, "window", { value: dom.window, configurable: true });
Object.defineProperty(globalThis, "document", { value: dom.window.document, configurable: true });
Object.defineProperty(globalThis, "navigator", { value: dom.window.navigator, configurable: true });

const { cleanup, render, screen } = await import("@testing-library/react");
const { default: EmptyState } = await import("../src/components/EmptyState.tsx");

afterEach(() => cleanup());

test("EmptyState renders user-visible description and optional action", () => {
  render(React.createElement(EmptyState, {
    kind: "models",
    description: React.createElement("span", null, "No models yet"),
    action: React.createElement("button", { type: "button" }, "Add model"),
  }));
  assert.equal(screen.getByText("No models yet").textContent, "No models yet");
  assert.equal(screen.getByRole("button", { name: "Add model" }).textContent, "Add model");
  assert.equal(document.querySelector('[data-slot="empty-media"] img')?.getAttribute("src"), "/assets/empty-models.webp");
});

test("EmptyState omits action and applies compact class", () => {
  const { container } = render(React.createElement(EmptyState, {
    description: "Nothing here",
    compact: true,
  }));
  assert.equal(container.firstElementChild?.getAttribute("data-density"), "compact");
  assert.equal(container.firstElementChild?.getAttribute("data-scope"), "inline");
  assert.equal(screen.queryByRole("button"), null);
});

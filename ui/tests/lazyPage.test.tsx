// A page is its own chunk. After a server upgrade a tab that stayed open asks
// for chunks that no longer exist: the console is loaded again once, and a
// page that still cannot be loaded goes to the error boundary.
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React, { Component, Suspense, type ReactNode } from "react";
import { JSDOM } from "jsdom";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/deployments" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "sessionStorage"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const { lazyPage } = await import("../src/app/lazyPage.ts");

afterEach(() => { cleanup(); dom.window.sessionStorage.clear(); });

class Boundary extends Component<{ children: ReactNode }, { failed: boolean }> {
  state = { failed: false };
  static getDerivedStateFromError() { return { failed: true }; }
  render() { return this.state.failed ? <p>could not load</p> : this.props.children; }
}

function mount(Page: React.ComponentType) {
  return render(<Boundary><Suspense fallback={<p>loading</p>}><Page /></Suspense></Boundary>);
}

const gone = () => Promise.reject(new TypeError("Failed to fetch dynamically imported module"));

test("a page shows its loading state, then itself", async () => {
  let reloads = 0;
  const Page = lazyPage(async () => ({ default: () => <h1>Deployments</h1> }), () => { reloads++; });
  mount(Page);
  assert.ok(screen.getByText("loading"));
  assert.ok(await screen.findByRole("heading", { name: "Deployments" }));
  assert.equal(reloads, 0);
});

test("a chunk that is gone reloads the console once; failing again shows the error", async () => {
  let reloads = 0;
  mount(lazyPage(gone, () => { reloads++; }));
  await waitFor(() => assert.equal(reloads, 1));
  // The reload is under way: the page keeps its loading state and reports no error.
  assert.ok(screen.getByText("loading"));
  assert.equal(screen.queryByText("could not load"), null);
  cleanup();

  // The reloaded console asks for the same page and the chunk is still gone.
  mount(lazyPage(gone, () => { reloads++; }));
  assert.ok(await screen.findByText("could not load"));
  assert.equal(reloads, 1, "no second reload for the same page");
});

test("a page that loads clears the mark, so a later upgrade reloads again", async () => {
  let reloads = 0;
  mount(lazyPage(gone, () => { reloads++; }));
  await waitFor(() => assert.equal(reloads, 1));
  cleanup();
  mount(lazyPage(async () => ({ default: () => <h1>Deployments</h1> }), () => { reloads++; }));
  await screen.findByRole("heading", { name: "Deployments" });
  cleanup();
  mount(lazyPage(gone, () => { reloads++; }));
  await waitFor(() => assert.equal(reloads, 2));
});

test("a browser that refuses the mark goes to the error instead of reloading", async () => {
  let reloads = 0;
  const setItem = dom.window.Storage.prototype.setItem;
  dom.window.Storage.prototype.setItem = () => { throw new Error("QuotaExceededError"); };
  try {
    mount(lazyPage(gone, () => { reloads++; }));
    assert.ok(await screen.findByText("could not load"));
    assert.equal(reloads, 0);
  } finally {
    dom.window.Storage.prototype.setItem = setItem;
  }
});

// The session boundary reads the signed-in user once from /auth/me. A page
// that asks whether the user is an admin uses that answer; only outside the
// boundary (a page mounted alone) does it ask /auth/me itself.
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { JSDOM } from "jsdom";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { SessionUserContext } = await import("../src/components/sessionUser.ts");
const { useIsAdmin } = await import("../src/components/useIsAdmin.ts");

const originalMe = api.me;
afterEach(() => { cleanup(); api.me = originalMe; });

function Probe({ enabled = true }: { enabled?: boolean }) {
  return <p>{useIsAdmin(enabled) ? "admin" : "not admin"}</p>;
}

function countMe(role: string) {
  let calls = 0;
  api.me = (async () => { calls++; return { id: 7, username: "ops", role }; }) as never;
  return () => calls;
}

test("the session's user answers without another /auth/me request", async () => {
  const calls = countMe("member");
  for (const [role, text] of [["admin", "admin"], ["member", "not admin"]] as const) {
    render(<SessionUserContext.Provider value={{ id: 7, username: "ops", role }}><Probe /></SessionUserContext.Provider>);
    assert.ok(screen.getByText(text));
    cleanup();
  }
  // Give a request that should not exist the time to be made.
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(calls(), 0);
});

test("without a session user /auth/me is asked once, and a failed lookup is not an admin", async () => {
  const calls = countMe("admin");
  render(<Probe />);
  assert.ok(screen.getByText("not admin"), "not an admin until the answer arrives");
  await waitFor(() => assert.ok(screen.getByText("admin")));
  assert.equal(calls(), 1);
  cleanup();

  api.me = (async () => { throw new Error("401"); }) as never;
  render(<Probe />);
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.ok(screen.getByText("not admin"));
});

test("a disabled check asks nothing and is not an admin, whoever is signed in", async () => {
  const calls = countMe("admin");
  render(<Probe enabled={false} />);
  render(<SessionUserContext.Provider value={{ id: 7, username: "ops", role: "admin" }}><Probe enabled={false} /></SessionUserContext.Provider>);
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(screen.getAllByText("not admin").length, 2);
  assert.equal(calls(), 0);
});

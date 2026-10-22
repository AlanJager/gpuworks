import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "Element", "Node", "MutationObserver"] as const) {
	Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}

const { cleanup, render, act } = await import("@testing-library/react");
const { usePolling } = await import("../src/api.ts");

afterEach(() => cleanup());

test("usePolling does not restart its effect after state updates when deps are omitted", async () => {
	let calls = 0;
	function Probe() {
		usePolling(async () => {
			calls += 1;
			return calls;
		}, 0);
		return null;
	}

	render(<Probe />);
	await act(async () => { await Promise.resolve(); await Promise.resolve(); });
	assert.equal(calls, 1);
});

import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
	Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });

const i18n = createInstance();
await i18n.use(initReactI18next).init({
	lng: "zh", defaultNS: "nodes", interpolation: { escapeValue: false },
	resources: { zh: { nodes: { upgrade: { action: "升级 Agent", waitingReconnect: "等待重连" } } } },
});

const { act, cleanup, fireEvent, render, screen } = await import("@testing-library/react");
const { evaluateUpgrade, useAgentUpgrades, UPGRADE_TIMEOUT_MS } = await import("../src/agentUpgrade.ts");
const { default: AgentUpgradeButton } = await import("../src/components/AgentUpgradeButton.tsx");
type NodeT = import("../src/api.ts").Node;
type UpgradeEventT = import("../src/agentUpgrade.ts").UpgradeEvent;
type PendingUpgradeT = import("../src/agentUpgrade.ts").PendingUpgrade;

afterEach(() => cleanup());

const T0 = Date.parse("2026-09-25T10:00:00Z");
const iso = (ms: number) => new Date(ms).toISOString();

function node(uuid: string, over: Partial<NodeT> = {}): NodeT {
	return { uuid, hostname: `host-${uuid}`, state: "online", agent_version: "v1", last_seen_at: iso(T0), ...over } as NodeT;
}

const waiting: PendingUpgradeT = { phase: "waiting", name: "host-a", startedAt: T0, baselineSeen: T0 };

test("evaluateUpgrade decides each outcome from the node list, error map and clock", () => {
	const later = iso(T0 + 1000);
	const cases: [string, Parameters<typeof evaluateUpgrade>, string][] = [
		["reconnected with the server version", [waiting, node("a", { agent_version: "v2", last_seen_at: later }), "v2", undefined, T0 + 1000], "completed"],
		["old version still reporting", [waiting, node("a", { last_seen_at: later }), "v2", undefined, T0 + 1000], "pending"],
		["new version but offline", [waiting, node("a", { agent_version: "v2", state: "offline", last_seen_at: later }), "v2", undefined, T0 + 1000], "pending"],
		["new version not seen since the click", [waiting, node("a", { agent_version: "v2" }), "v2", undefined, T0 + 1000], "pending"],
		["dev server never completes", [waiting, node("a", { agent_version: "dev", last_seen_at: later }), "dev", undefined, T0 + 1000], "pending"],
		["unknown server version never completes", [waiting, node("a", { agent_version: "v2", last_seen_at: later }), undefined, undefined, T0 + 1000], "pending"],
		["node missing from the list", [waiting, undefined, "v2", undefined, T0 + 1000], "pending"],
		["new error after the click", [waiting, node("a"), "v2", "smoke failed", T0 + 1000], "failed"],
		["error left over from an earlier attempt", [{ ...waiting, baselineError: "old" }, node("a"), "v2", "old", T0 + 1000], "pending"],
		["different error than the leftover one", [{ ...waiting, baselineError: "old" }, node("a"), "v2", "new", T0 + 1000], "failed"],
		["just before the timeout", [waiting, node("a"), "v2", undefined, T0 + UPGRADE_TIMEOUT_MS - 1], "pending"],
		["at the timeout", [waiting, node("a"), "v2", undefined, T0 + UPGRADE_TIMEOUT_MS], "timeout"],
		["completion wins over the timeout", [waiting, node("a", { agent_version: "v2", last_seen_at: later }), "v2", undefined, T0 + UPGRADE_TIMEOUT_MS], "completed"],
	];
	for (const [name, args, want] of cases) assert.equal(evaluateUpgrade(...args), want, name);
});

type Props = { nodes: NodeT[]; errors?: Record<string, string>; version?: string };

function harness(upgradeNode: (uuid: string) => Promise<unknown>) {
	const events: UpgradeEventT[] = [];
	const clock = { now: T0 };
	let api: ReturnType<typeof useAgentUpgrades> | undefined;
	function Probe({ nodes, errors = {}, version = "v2" }: Props) {
		api = useAgentUpgrades({ nodes, serverVersion: version, upgradeErrors: errors, onEvent: (e) => events.push(e), upgradeNode, now: () => clock.now });
		return null;
	}
	const view = render(<Probe nodes={[node("a"), node("b")]} />);
	return {
		events, clock,
		get api() { return api!; },
		update: (props: Props) => view.rerender(<Probe {...props} />),
	};
}

test("a request failure clears the node so it can be retried", async () => {
	let calls = 0;
	const h = harness(async () => { calls += 1; if (calls === 1) throw new Error("node is offline"); });
	await act(async () => { await h.api.start(node("a")); });
	assert.equal(h.api.phaseOf("a"), undefined);
	assert.deepEqual(h.events, [{ kind: "requestFailed", uuid: "a", name: "host-a", detail: "node is offline" }]);
	await act(async () => { await h.api.start(node("a")); });
	assert.equal(calls, 2);
	assert.equal(h.api.phaseOf("a"), "waiting");
});

test("the button stays busy while the request is in flight and ignores repeat clicks", async () => {
	let resolve!: () => void;
	const calls: string[] = [];
	const h = harness((uuid) => { calls.push(uuid); return new Promise<void>((r) => { resolve = r; }); });
	let first!: Promise<void>;
	act(() => { first = h.api.start(node("a")); void h.api.start(node("a")); });
	assert.equal(h.api.phaseOf("a"), "requesting");
	assert.deepEqual(calls, ["a"]);
	await act(async () => { resolve(); await first; });
	assert.equal(h.api.phaseOf("a"), "waiting");
	assert.deepEqual(h.events.map((e) => e.kind), ["dispatched"]);
});

test("one node upgrading does not block another, and completes without its drawer open", async () => {
	const h = harness(async () => ({ status: "upgrading" }));
	await act(async () => { await h.api.start(node("a")); });
	assert.equal(h.api.phaseOf("a"), "waiting");
	assert.equal(h.api.phaseOf("b"), undefined);
	await act(async () => { await h.api.start(node("b")); });
	assert.equal(h.api.phaseOf("b"), "waiting");

	// Only the list data changes; nothing renders a detail view for A.
	const upgradedA = node("a", { agent_version: "v2", last_seen_at: iso(T0 + 5000) });
	h.clock.now = T0 + 5000;
	act(() => h.update({ nodes: [upgradedA, node("b")] }));
	assert.equal(h.api.phaseOf("a"), undefined);
	assert.equal(h.api.phaseOf("b"), "waiting");
	assert.deepEqual(h.events.at(-1), { kind: "completed", uuid: "a", name: "host-a", node: upgradedA });
});

test("an asynchronous agent failure and a timeout both release the node", async () => {
	const h = harness(async () => ({}));
	await act(async () => { await h.api.start(node("a")); await h.api.start(node("b")); });
	act(() => h.update({ nodes: [node("a"), node("b")], errors: { a: "download agent: http 404" } }));
	assert.equal(h.api.phaseOf("a"), undefined);
	assert.deepEqual(h.events.at(-1), { kind: "failed", uuid: "a", name: "host-a", detail: "download agent: http 404" });

	h.clock.now = T0 + UPGRADE_TIMEOUT_MS;
	act(() => h.update({ nodes: [node("a"), node("b")], errors: { a: "download agent: http 404" } }));
	assert.equal(h.api.phaseOf("b"), undefined);
	assert.deepEqual(h.events.at(-1), { kind: "timeout", uuid: "b", name: "host-b" });
});

test("an error left over from an earlier attempt does not fail a retry", async () => {
	const h = harness(async () => ({}));
	act(() => h.update({ nodes: [node("a")], errors: { a: "old failure" } }));
	await act(async () => { await h.api.start(node("a")); });
	act(() => h.update({ nodes: [node("a")], errors: { a: "old failure" } }));
	assert.equal(h.api.phaseOf("a"), "waiting");
});

function renderButton(props: Partial<React.ComponentProps<typeof AgentUpgradeButton>> & { onUpgrade?: () => void }) {
	return render(<I18nextProvider i18n={i18n}><AgentUpgradeButton online onUpgrade={() => {}} {...props} /></I18nextProvider>);
}

test("upgrade button is a focusable native button that only fires when idle", () => {
	let clicks = 0;
	const onUpgrade = () => { clicks += 1; };
	const { rerender } = renderButton({ onUpgrade });
	const button = screen.getByRole("button", { name: /升级 Agent/ });
	// A native <button> gets Enter/Space activation from the browser.
	assert.equal(button.tagName, "BUTTON");
	button.focus();
	assert.equal(document.activeElement, button);
	fireEvent.click(button);
	assert.equal(clicks, 1);

	rerender(<I18nextProvider i18n={i18n}><AgentUpgradeButton online phase="requesting" onUpgrade={onUpgrade} /></I18nextProvider>);
	fireEvent.click(screen.getByRole("button", { name: /升级 Agent/ }));
	assert.equal(clicks, 1);
});

test("upgrade button shows the waiting state and is disabled while waiting or offline", () => {
	const { rerender } = renderButton({ phase: "waiting" });
	const waitingButton = screen.getByRole("button", { name: /等待重连/ });
	assert.equal((waitingButton as HTMLButtonElement).disabled, true);
	assert.ok(waitingButton.className.includes("ant-btn-loading"));

	rerender(<I18nextProvider i18n={i18n}><AgentUpgradeButton online phase="requesting" onUpgrade={() => {}} /></I18nextProvider>);
	const requesting = screen.getByRole("button", { name: /升级 Agent/ });
	assert.ok(requesting.className.includes("ant-btn-loading"));

	rerender(<I18nextProvider i18n={i18n}><AgentUpgradeButton online={false} onUpgrade={() => {}} /></I18nextProvider>);
	assert.equal((screen.getByRole("button", { name: /升级 Agent/ }) as HTMLButtonElement).disabled, true);
});

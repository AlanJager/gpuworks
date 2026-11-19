import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { api, type AgentUpgradePreview } from "../src/api.ts";

const { cleanup, fireEvent, render, screen, waitFor, act } = await import("@testing-library/react");
const AgentUpgradeButton = (await import("../src/components/AgentUpgradeButton.tsx")).default;
const originalPreview = api.agentUpgradePreview;
afterEach(() => { cleanup(); api.agentUpgradePreview = originalPreview; });
const preview = (eligibility: string): AgentUpgradePreview => ({ target_version: "same-release-other-build", manifest_fingerprint: "artifact-digest", node_uuids: ["node-a"], counts: { [eligibility]: 1 }, nodes: [{ node_uuid: "node-a", name: "host-a", from_version: "same-release", os: "linux", arch: "amd64", transport: "push", eligibility, version_change: "different_build" }] });
async function mount(eligibility?: string) {
  const { i18n } = await createI18n(["nodes"], "en");
  let starts = 0;
  const props = { nodeUUID: "node-a", online: true, eligibility, onUpgrade: () => { starts++; } };
  const view = render(<I18nextProvider i18n={i18n}><AgentUpgradeButton {...props} /></I18nextProvider>);
  return { get starts() { return starts; }, rerender: (extra: Partial<typeof props>) => view.rerender(<I18nextProvider i18n={i18n}><AgentUpgradeButton {...props} {...extra} /></I18nextProvider>) };
}

test("an already-current node displays its server eligibility without an upgrade request", async () => {
  let checks = 0; api.agentUpgradePreview = async () => { checks++; return preview("already_current"); };
  const h = await mount("already_current");
  const button = screen.getByRole("button", { name: /Already at target/ });
  assert.equal((button as HTMLButtonElement).disabled, true);
  fireEvent.click(button);
  assert.equal(h.starts, 0); assert.equal(checks, 0);
});

test("a different build remains eligible and is checked again before dispatch", async () => {
  const requests: unknown[] = [];
  api.agentUpgradePreview = async (request) => { requests.push(request); return preview("eligible"); };
  const h = await mount("eligible");
  fireEvent.click(screen.getByRole("button", { name: /Upgrade Agent/ }));
  await waitFor(() => assert.equal(h.starts, 1));
  assert.deepEqual(requests, [{ node_uuids: ["node-a"] }]);
});

test("eligibility becoming current during preview never dispatches and explains the result", async () => {
  let resolve!: (value: AgentUpgradePreview) => void;
  api.agentUpgradePreview = () => new Promise((r) => { resolve = r; });
  const h = await mount("eligible");
  const button = screen.getByRole("button", { name: /Upgrade Agent/ });
  fireEvent.click(button); fireEvent.click(button);
  await act(async () => resolve(preview("already_current")));
  assert.equal(h.starts, 0);
  assert.equal((screen.getByRole("button", { name: /Already at target/ }) as HTMLButtonElement).disabled, true);
});

test("an unknown eligibility is checked, real preview errors are visible and retry works", async () => {
  let calls = 0;
  api.agentUpgradePreview = async () => { if (++calls === 1) throw new Error("artifact metadata unavailable"); return preview("eligible"); };
  const h = await mount();
  fireEvent.click(screen.getByRole("button", { name: /Upgrade Agent/ }));
  await screen.findByRole("alert");
  assert.match(screen.getByRole("alert").textContent ?? "", /artifact metadata unavailable/);
  assert.equal(h.starts, 0);
  fireEvent.click(screen.getByRole("button", { name: /Upgrade Agent/ }));
  await waitFor(() => assert.equal(h.starts, 1));
});

test("an obsolete preview for a different node cannot start an upgrade", async () => {
  let resolve!: (value: AgentUpgradePreview) => void;
  api.agentUpgradePreview = () => new Promise((r) => { resolve = r; });
  const h = await mount("eligible");
  fireEvent.click(screen.getByRole("button", { name: /Upgrade Agent/ }));
  h.rerender({ nodeUUID: "node-b" });
  await act(async () => resolve(preview("eligible")));
  assert.equal(h.starts, 0);
});

for (const eligibility of ["offline", "unknown_version", "unsupported", "busy"]) {
  test(`server ${eligibility} eligibility prevents dispatch`, async () => {
    let checks = 0;
    api.agentUpgradePreview = async () => { checks++; return preview("eligible"); };
    const h = await mount(eligibility);
    const button = screen.getByRole("button", { name: /Upgrade Agent/ });
    assert.equal((button as HTMLButtonElement).disabled, true);
    fireEvent.click(button); assert.equal(h.starts, 0); assert.equal(checks, 0);
  });
}

test("the real preview HTTP boundary checks only the selected node with authenticated JSON", async () => {
  const originalFetch = globalThis.fetch;
  const requests: Array<{ path: string; init?: RequestInit }> = [];
  globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
    requests.push({ path: String(input), init });
    return new Response(JSON.stringify(preview("already_current")), { headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  try {
    api.agentUpgradePreview = originalPreview;
    const h = await mount("eligible");
    fireEvent.click(screen.getByRole("button", { name: /Upgrade Agent/ }));
    await screen.findByRole("button", { name: /Already at target/ });
    assert.equal(h.starts, 0);
    assert.equal(requests.length, 1);
    assert.equal(requests[0].path, "/api/v1/agent-upgrades/preview");
    assert.equal(requests[0].init?.credentials, "include");
    assert.deepEqual(JSON.parse(String(requests[0].init?.body)), { node_uuids: ["node-a"] });
  } finally { globalThis.fetch = originalFetch; }
});

test("a global blocker on another node can be retried after release without changing this node", async () => {
  let blocked = true;
  let checks = 0;
  api.agentUpgradePreview = async () => {
    checks++;
    return { ...preview("eligible"), blocked_by_item: blocked ? { run_id: 7, node_uuid: "other-node", name: "other-host", reason: "active" } : null };
  };
  const h = await mount("eligible");
  fireEvent.click(screen.getByRole("button", { name: /Upgrade Agent/ }));
  await waitFor(() => assert.equal(checks, 1));
  await waitFor(() => assert.equal((screen.getByRole("button", { name: /Upgrade Agent/ }) as HTMLButtonElement).disabled, false));
  assert.equal(h.starts, 0);
  assert.match(screen.getByRole("status").textContent ?? "", /other-host.*active upgrade batch/);
  blocked = false;
  fireEvent.click(screen.getByRole("button", { name: /Upgrade Agent/ }));
  await waitFor(() => assert.equal(h.starts, 1));
  assert.equal(checks, 2);
});

test("temporary preview artifact unavailability can recover without changing node props", async () => {
  let unavailable = true;
  api.agentUpgradePreview = async () => preview(unavailable ? "unsupported" : "eligible");
  const h = await mount("eligible");
  fireEvent.click(screen.getByRole("button", { name: /Upgrade Agent/ }));
  await screen.findByRole("status");
  assert.match(screen.getByRole("status").textContent ?? "", /platform or upgrade method is not supported/);
  assert.equal(h.starts, 0);
  assert.equal((screen.getByRole("button", { name: /Upgrade Agent/ }) as HTMLButtonElement).disabled, false);
  unavailable = false;
  fireEvent.click(screen.getByRole("button", { name: /Upgrade Agent/ }));
  await waitFor(() => assert.equal(h.starts, 1));
});

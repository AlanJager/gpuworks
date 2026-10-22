import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { readFileSync } from "node:fs";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of [
  "window", "document", "navigator", "HTMLElement", "HTMLBodyElement", "SVGElement", "Element", "Node",
  "MutationObserver", "ShadowRoot", "getComputedStyle", "matchMedia", "requestAnimationFrame",
] as const) {
  if (dom.window[name] === undefined) continue;
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }),
});
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const load = (ns: string) => JSON.parse(readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const common = load("common");
const providers = load("providers");

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  defaultNS: "common",
  resources: { en: { common, providers } },
  interpolation: { escapeValue: false },
});

const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { default: ProviderInstallModal } = await import("../src/components/ProviderInstallModal");
const { installVerdict } = await import("../src/providerInstallOutcome");
const apiModule = await import("../src/api");
const api = apiModule.api as Record<string, unknown>;

const restores: Array<() => void> = [];
function stub(name: string, impl: (...args: any[]) => unknown) {
  const original = api[name];
  const calls: any[][] = [];
  api[name] = async (...args: any[]) => {
    calls.push(args);
    return impl(...args);
  };
  restores.push(() => { api[name] = original; });
  return calls;
}
afterEach(() => {
  cleanup();
  while (restores.length) restores.pop()!();
});

const instance = { external_id: "pod-1", name: "pod-1", public_ip: "203.0.113.30", ssh_connections: [] } as never;
const o = providers.install.outcome;

function open(callbacks: { onClose?: () => void; onDone?: () => void } = {}) {
  const calls = { closed: 0, done: 0 };
  render(
    <I18nextProvider i18n={i18n}>
      <ProviderInstallModal
        providerId={7}
        providerKind="runpod"
        inst={instance}
        onClose={() => { calls.closed++; callbacks.onClose?.(); }}
        onDone={() => { calls.done++; callbacks.onDone?.(); }}
      />
    </I18nextProvider>,
  );
  return calls;
}

// A button that has just finished loading still carries antd's spinner icon
// (named "loading") while it animates out, so match the label at the end.
const label = (name: string) => new RegExp(`^(loading)?${name}$`);
const press = (name: string) => fireEvent.click(screen.getByRole("button", { name: label(name) }));
const hasButton = (name: string) => screen.queryByRole("button", { name: label(name) }) !== null;

const okChecks = [
  { name: "precheck", status: "ok", detail: "all 17 checks passed" },
  { name: "install agent", status: "ok", detail: "agent process is running" },
  { name: "agent_dial", status: "ok", detail: "agent session established" },
];

test("a finished install says so, and leaves only Done", async () => {
  const installs = stub("installAgent", () => ({ status: "installing", connect_mode: "agent_dial", checks: okChecks }));
  const calls = open();
  press(providers.install.install);
  await waitFor(() => assert.ok(screen.getByText(o.installed.title)));
  assert.ok(screen.getByText(o.installed.description));
  assert.equal(installs.length, 1);
  // The form and the Install button are gone: pressing Install again would
  // replace the agent and create a second node.
  assert.equal(hasButton(providers.install.install), false);
  assert.equal(hasButton(providers.install.retry), false);
  assert.equal(screen.queryByText(providers.install.autoAuth.runpod), null);
  assert.equal(calls.done, 0, "the dialog stays open on the result until the operator closes it");
  press(providers.install.done);
  assert.equal(calls.done, 1);
  assert.equal(calls.closed, 0);
});

test("closing a finished install with the window's close button also refreshes the list", async () => {
  stub("installAgent", () => ({ status: "installing", connect_mode: "agent_dial", checks: okChecks }));
  const calls = open();
  press(providers.install.install);
  await waitFor(() => assert.ok(screen.getByText(o.installed.title)));
  fireEvent.click(screen.getByRole("button", { name: "Close" }));
  assert.equal(calls.done, 1);
  assert.equal(calls.closed, 0);
});

test("a precheck warning is a note on a finished install, not a failure", async () => {
  stub("installAgent", () => ({
    status: "installing", connect_mode: "agent_dial",
    checks: [{ name: "precheck", status: "warn", detail: "Install directory free space: 20.0 GiB free of 80.0 GiB" }, ...okChecks.slice(1)],
    precheck_details: [
      { name: "OS/Arch", status: "ok", detail: "Linux x86_64" },
      { name: "Install directory free space", status: "warn", detail: "20.0 GiB free of 80.0 GiB" },
    ],
  }));
  const calls = open();
  press(providers.install.install);
  await waitFor(() => assert.ok(screen.getByText(o.installedWithNotes.title)));
  assert.ok(screen.getByText(o.installedWithNotes.description));
  assert.equal(screen.queryByText(o.failed.title), null);
  // The summary row names the warning only; the passing items are one click away.
  assert.ok(screen.getByText("Install directory free space: 20.0 GiB free of 80.0 GiB"));
  assert.equal(screen.queryByText("Linux x86_64"), null);
  fireEvent.click(screen.getByText(providers.install.precheckDetails));
  await waitFor(() => assert.ok(screen.getByText("Linux x86_64")));
  press(providers.install.done);
  assert.equal(calls.done, 1);
});

test("an agent that started but has not connected is reported as pending", async () => {
  stub("installAgent", () => ({
    status: "installing", connect_mode: "agent_dial",
    checks: [okChecks[0], okChecks[1], { name: "agent_dial", status: "warn", detail: "agent started; waiting for its first session" }],
  }));
  const calls = open();
  press(providers.install.install);
  await waitFor(() => assert.ok(screen.getByText(o.waitingForAgent.title)));
  assert.ok(screen.getByText(o.waitingForAgent.description));
  assert.equal(hasButton(providers.install.install), false);
  press(providers.install.done);
  assert.equal(calls.done, 1, "the node exists, so the list has to refresh");
});

test("a server that cannot reach the agent yet points at the firewall, not the agent's way out", async () => {
  stub("installAgent", () => ({
    status: "installing", connect_mode: "server_dial",
    checks: [okChecks[0], okChecks[1], { name: "server_dial", status: "warn", detail: "queued for reconnection: i/o timeout" }],
  }));
  open();
  press(providers.install.install);
  await waitFor(() => assert.ok(screen.getByText(o.waitingForServer.title)));
  assert.ok(screen.getByText(o.waitingForServer.description));
  assert.equal(screen.queryByText(o.waitingForAgent.title), null);
});

test("a rejected request stays on screen as an error with the reason, and can be retried", async () => {
  let attempt = 0;
  const installs = stub("installAgent", () => {
    attempt++;
    if (attempt === 1) throw new Error("ssh: unable to authenticate; the target has not authorized the platform key");
    return { status: "installing", connect_mode: "agent_dial", checks: okChecks };
  });
  const calls = open();
  press(providers.install.install);
  await waitFor(() => assert.ok(screen.getByText(o.failed.title)));
  assert.ok(screen.getByText("ssh: unable to authenticate; the target has not authorized the platform key"));
  // The form is still there to fix the cause, and Install became Try again.
  assert.ok(screen.getByText(providers.install.autoAuth.runpod));
  assert.equal(hasButton(providers.install.install), false);
  assert.equal(hasButton(providers.install.done), false);

  press(providers.install.retry);
  await waitFor(() => assert.ok(screen.getByText(o.installed.title)));
  assert.equal(installs.length, 2);
  assert.equal(screen.queryByText(o.failed.title), null, "the old error went away with the retry");
  assert.equal(calls.done, 0);
  assert.equal(calls.closed, 0);
});

test("cancelling after a failure closes without refreshing", async () => {
  stub("installAgent", () => { throw new Error("boom"); });
  const calls = open();
  press(providers.install.install);
  await waitFor(() => assert.ok(screen.getByText(o.failed.title)));
  press(providers.install.cancel);
  assert.equal(calls.closed, 1);
  assert.equal(calls.done, 0);
});

test("an install whose agent did not start is a failure even though the request succeeded", async () => {
  stub("installAgent", () => ({
    status: "installing", connect_mode: "agent_dial",
    checks: [okChecks[0], { name: "install agent", status: "error", detail: "agent process exited: bad token" }, { name: "agent_dial", status: "warn", detail: "pending node UUID is empty" }],
  }));
  const calls = open();
  press(providers.install.install);
  await waitFor(() => assert.ok(screen.getByText(o.failed.title)));
  assert.ok(screen.getAllByText("agent process exited: bad token").length >= 1);
  assert.ok(hasButton(providers.install.retry));
  assert.equal(calls.done, 0);
});

test("the dialog says it is working while the install runs", async () => {
  let finish: (value: unknown) => void = () => {};
  stub("installAgent", () => new Promise((resolve) => { finish = resolve; }));
  open();
  press(providers.install.install);
  await waitFor(() => assert.ok(screen.getByText(providers.install.running)));
  assert.equal(screen.queryByText(o.installed.title), null);
  finish({ status: "installing", connect_mode: "agent_dial", checks: okChecks });
  await waitFor(() => assert.ok(screen.getByText(o.installed.title)));
  assert.equal(screen.queryByText(providers.install.running), null);
});

test("installVerdict reads the deciding rows and ignores the host's precheck", () => {
  const base = { status: "installing", connect_mode: "agent_dial" as const };
  assert.equal(installVerdict(null, null), null);
  assert.deepEqual(installVerdict(null, "boom"), { outcome: "failed", detail: "boom" });
  assert.equal(installVerdict({ ...base, checks: [] }, null)?.outcome, "installed");
  assert.equal(installVerdict({ ...base }, null)?.outcome, "installed");
  // A precheck error does not mean the install did not happen: the install goes on after it.
  const precheckError = { name: "precheck", status: "error" as const, detail: "SSH: refused" };
  assert.equal(installVerdict({ ...base, checks: [precheckError, okChecks[1], okChecks[2]] } as never, null)?.outcome, "installedWithNotes");
  assert.equal(installVerdict({ ...base, checks: [okChecks[0], { name: "server_dial", status: "warn", detail: "queued" }] } as never, null)?.outcome, "waitingForServer");
  assert.equal(installVerdict({ ...base, checks: [{ name: "agent_dial", status: "error", detail: "x" }] } as never, null)?.outcome, "failed");
});

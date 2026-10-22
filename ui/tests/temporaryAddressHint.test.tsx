import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { readFileSync } from "node:fs";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of [
  "window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node",
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

const common = JSON.parse(readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8"));
const nodes = JSON.parse(readFileSync(new URL("../src/i18n/locales/en/nodes.json", import.meta.url), "utf8"));

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  defaultNS: "common",
  resources: { en: { common, nodes } },
  interpolation: { escapeValue: false },
});

const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { default: TemporaryAddressHint, isTemporaryAddress } = await import("../src/components/TemporaryAddressHint");
const { default: AddNodeDrawer } = await import("../src/components/AddNodeDrawer");
const apiModule = await import("../src/api");

afterEach(() => cleanup());

function renderHint(value: string) {
  return render(
    <I18nextProvider i18n={i18n}>
      <TemporaryAddressHint value={value} />
    </I18nextProvider>,
  );
}

// The shapes an operator actually pastes, including the ones that must NOT be
// treated as a quick tunnel: a lookalike suffix is somebody else's host.
test("temporary addresses are recognised in the spellings operators paste", () => {
  const yes = [
    "https://silly-words-here.trycloudflare.com",
    "silly-words-here.trycloudflare.com",
    "SILLY-WORDS-HERE.TRYCLOUDFLARE.COM",
    "silly-words-here.trycloudflare.com:8800",
    "https://silly-words-here.trycloudflare.com/healthz",
    "silly-words-here.trycloudflare.com.", // fully-qualified form
    "trycloudflare.com",
  ];
  for (const value of yes) {
    assert.equal(isTemporaryAddress(value), true, `${value} should be recognised`);
  }

  const no = [
    "https://gpu.example.com",
    "https://x.trycloudflare.com.example.com", // lookalike suffix
    "https://trycloudflare.com.evil.example",
    "https://not-atrycloudflare.com",
    "https://gpu.example.com:8800",
    "",
    "   ",
    "not a url at all",
    ":8800",
  ];
  for (const value of no) {
    assert.equal(isTemporaryAddress(value), false, `${value} should not be recognised`);
  }
});

test("the hint appears for a temporary address and disappears for a stable one", async () => {
  const { rerender } = renderHint("https://silly-words-here.trycloudflare.com");
  assert.ok(screen.getByText(/looks like a Cloudflare quick tunnel address/i));

  rerender(
    <I18nextProvider i18n={i18n}>
      <TemporaryAddressHint value="https://gpu.example.com" />
    </I18nextProvider>,
  );
  assert.equal(screen.queryByText(/looks like a Cloudflare quick tunnel address/i), null);
});

// The manual add-node form, in its default connection direction: the address
// the operator types there is the one the node is told to dial, and it is the
// one that has to survive the connector restarting.
test("the add-node form warns about the address the node will dial", async () => {
  render(
    <I18nextProvider i18n={i18n}>
      <AddNodeDrawer open onClose={() => {}} onInstalled={() => {}} />
    </I18nextProvider>,
  );

  // fireEvent, not userEvent: antd's Drawer reports pointer-events: none in
  // jsdom until its animation runs, which userEvent refuses to click through.
  // What is under test is the hint's presence on input, not pointer plumbing.
  // The server-address field starts out holding this console's own address, so
  // that value is what names it.
  const address = screen.getByDisplayValue(window.location.origin);
  fireEvent.change(address, { target: { value: "https://silly-words-here.trycloudflare.com" } });
  assert.ok(screen.getByText(/looks like a Cloudflare quick tunnel address/i));
  assert.ok(screen.getByText(/will stop reconnecting/i));

  // Switching to a stable address clears the warning; nothing was blocked on
  // the way here, and no submit button was disabled by the hint.
  fireEvent.change(address, { target: { value: "https://gpu.example.com" } });
  assert.equal(screen.queryByText(/looks like a Cloudflare quick tunnel address/i), null);
});

// The hint follows the field that carries the address, not the other one: the
// agent's own address is not something the node dials.
test("the agent's own address is not mistaken for the server's", async () => {
  render(
    <I18nextProvider i18n={i18n}>
      <AddNodeDrawer open onClose={() => {}} onInstalled={() => {}} />
    </I18nextProvider>,
  );
  fireEvent.click(screen.getByRole("radio", { name: nodes.connection.serverDial }));
  const agentAddress = screen.getByPlaceholderText(nodes.add.hostAddressPlaceholder);
  fireEvent.change(agentAddress, { target: { value: "https://silly-words-here.trycloudflare.com" } });
  assert.equal(screen.queryByText(/looks like a Cloudflare quick tunnel address/i), null);
  // A server-initiated node never dials the server, so its address is not asked.
  assert.equal(screen.queryByDisplayValue(window.location.origin), null);
});

// The server never guesses the address a node dials, so the form always sends
// one: what the operator typed, or this page's own origin when the field is
// left empty.
test("creating an install command always sends the address the node dials", async () => {
  const original = apiModule.api.createNode;
  const bodies: unknown[] = [];
  (apiModule.api as Record<string, unknown>).createNode = async (body: unknown) => {
    bodies.push(body);
    return { node: { uuid: "n1" }, token: "tok", install_command: "curl -fsSL http://localhost/install.sh | sh -s --" };
  };
  try {
    render(
      <I18nextProvider i18n={i18n}>
        <AddNodeDrawer open onClose={() => {}} onInstalled={() => {}} />
      </I18nextProvider>,
    );
    fireEvent.click(screen.getByRole("radio", { name: nodes.add.sshUnsupported }));
    fireEvent.change(screen.getByDisplayValue(window.location.origin), { target: { value: "  " } });
    fireEvent.click(screen.getByRole("button", { name: nodes.add.createCommand }));
    await waitFor(() => assert.equal(bodies.length, 1));
    assert.equal((bodies[0] as { server_url?: string }).server_url, "http://localhost");
  } finally {
    (apiModule.api as Record<string, unknown>).createNode = original;
  }
});

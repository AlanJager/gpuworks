import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://gpuworks.example/nodes" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLTextAreaElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }),
});
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const locale = (lang: string, ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lang}/${ns}.json`, import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: {
    en: { nodes: locale("en", "nodes"), common: locale("en", "common") },
    zh: { nodes: locale("zh", "nodes"), common: locale("zh", "common") },
  },
  defaultNS: "nodes",
  interpolation: { escapeValue: false },
});

const { act, cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { default: RejectedAgentsNotice, rejectedAgents } = await import("../src/components/RejectedAgentsNotice.tsx");
const { formatTime } = await import("../src/i18n/datetime.ts");

const original = api.agentRejects;
afterEach(async () => {
  await act(async () => { cleanup(); });
  api.agentRejects = original;
  await i18n.changeLanguage("en");
});

const en = locale("en", "nodes").rejects as Record<string, string>;
const minutesAgo = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString();
// What GET /api/v1/agent/rejects answers: newest first, the remote with the
// port of that attempt, the credential cut to the eight characters the server
// also logs.
const reject = (remote: string, prefix: string, minutes: number, reason = "bad_token") =>
  ({ token_prefix: prefix, reason, remote, at: minutesAgo(minutes) });
const list = <T,>(items: T[]) => ({ items, total: items.length });

const mount = (onAddNode?: () => void) =>
  render(<I18nextProvider i18n={i18n}><RejectedAgentsNotice onAddNode={onAddNode} /></I18nextProvider>);

test("sources are grouped by host and credential prefix, counted, and ordered by their latest attempt", () => {
  const now = Date.parse("2026-11-17T08:40:00Z");
  const at = (time: string) => `2026-11-17T${time}Z`;
  const groups = rejectedAgents([
    { token_prefix: "7830dac2", reason: "bad_token", remote: "172.25.17.88:50112", at: at("08:36:18") },
    { token_prefix: "99125708", reason: "bad_token", remote: "172.30.8.32:41000", at: at("08:36:17") },
    { token_prefix: "ae7d8b75", reason: "bad_token", remote: "172.25.17.88:50090", at: at("08:33:40") },
    { token_prefix: "7830dac2", reason: "bad_token", remote: "172.25.17.88:49800", at: at("08:31:18") },
    { token_prefix: "7830dac2", reason: "bad_token", remote: "[fd00::88]:49000", at: at("08:30:00") },
    { token_prefix: "0ld0ld0l", reason: "bad_token", remote: "172.30.8.31:40000", at: at("07:50:00") },
    { token_prefix: "d32933a0", reason: "stale_session", remote: "172.25.17.105:40000", at: at("08:36:00") },
    { token_prefix: "d32933a0", reason: "proto_mismatch", remote: "172.25.17.105:40001", at: at("08:36:01") },
    { token_prefix: "bad0bad0", reason: "bad_token", remote: "172.25.17.106:1", at: "not a time" },
  ], now);
  assert.deepEqual(groups, [
    { host: "172.25.17.88", tokenPrefix: "7830dac2", count: 2, first: at("08:31:18"), last: at("08:36:18") },
    { host: "172.30.8.32", tokenPrefix: "99125708", count: 1, first: at("08:36:17"), last: at("08:36:17") },
    { host: "172.25.17.88", tokenPrefix: "ae7d8b75", count: 1, first: at("08:33:40"), last: at("08:33:40") },
    { host: "fd00::88", tokenPrefix: "7830dac2", count: 1, first: at("08:30:00"), last: at("08:30:00") },
  ]);
});

test("nothing is shown while the answer is loading or when no Agent is refused", async () => {
  let answer: (value: ReturnType<typeof list>) => void = () => {};
  api.agentRejects = (() => new Promise((resolve) => { answer = resolve as never; })) as never;
  const view = mount();
  assert.equal(view.container.textContent, "", "loading shows nothing");
  await act(async () => { answer(list([])); });
  assert.equal(view.container.textContent, "", "an empty list shows nothing");
});

test("a refusal that is not about the credential, or is no longer recent, is not reported", async () => {
  api.agentRejects = (async () => list([
    reject("192.0.2.9:4000", "aaaaaaaa", 2, "stale_session"),
    reject("192.0.2.10:4000", "bbbbbbbb", 45),
  ])) as never;
  const view = mount();
  await act(async () => { await Promise.resolve(); });
  assert.equal(view.container.textContent, "");
});

test("refused Agents are named with host, credential prefix, attempts and times, and the next step", async () => {
  const events = [reject("192.0.2.230:41374", "891e71d7", 1), reject("192.0.2.230:41001", "891e71d7", 6), reject("192.0.2.231:5000", "", 3)];
  api.agentRejects = (async () => list(events)) as never;
  let opened = 0;
  mount(() => { opened += 1; });
  assert.ok(await screen.findByText(en.refused.replace("{{count}}", "2")));
  const rows = [...document.querySelectorAll("li")].map((li) => li.textContent);
  assert.deepEqual(rows, [
    `192.0.2.230 · credential 891e71d7… · 2 attempts · first ${formatTime(events[1].at)} · last ${formatTime(events[0].at)}`,
    `192.0.2.231 · credential ${en.emptyToken}… · 1 attempts · first ${formatTime(events[2].at)} · last ${formatTime(events[2].at)}`,
  ]);
  assert.ok(!document.body.textContent?.includes("41374"), "the per-attempt port is not shown");

  // Both controls are buttons a keyboard reaches: the explanation and the action.
  const help = screen.getByRole("button", { name: locale("en", "common").helpTip.label });
  help.focus();
  assert.equal(document.activeElement, help);
  const add = screen.getByRole("button", { name: locale("en", "nodes").action.add });
  add.focus();
  assert.equal(document.activeElement, add);
  fireEvent.click(add);
  assert.equal(opened, 1);
});

test("a viewer who cannot add nodes sees the refusals without the action", async () => {
  api.agentRejects = (async () => list([reject("192.0.2.230:41374", "891e71d7", 1)])) as never;
  mount();
  assert.ok(await screen.findByText(en.refused.replace("{{count}}", "1")));
  assert.equal(screen.queryByRole("button", { name: locale("en", "nodes").action.add }), null);
});

test("more sources than fit are counted, not listed", async () => {
  api.agentRejects = (async () => list(Array.from({ length: 7 }, (_, index) => reject(`192.0.2.${index + 1}:4000`, `tok${index}tok${index}`, index + 1)))) as never;
  mount();
  assert.ok(await screen.findByText(en.refused.replace("{{count}}", "7")));
  assert.equal(document.querySelectorAll("li").length, 6);
  assert.ok(screen.getByText(en.more.replace("{{count}}", "2")));
});

test("a list that cannot be read shows nothing and adds no second Retry to the page", async () => {
  let calls = 0;
  api.agentRejects = (async () => {
    calls += 1;
    throw new Error("boom");
  }) as never;
  const { container } = mount();
  await waitFor(() => assert.equal(calls, 1));
  await waitFor(() => assert.equal(container.textContent, ""));
  assert.equal(screen.queryByRole("button"), null);
});

test("the Chinese copy names the same facts", async () => {
  await i18n.changeLanguage("zh");
  const zh = locale("zh", "nodes").rejects as Record<string, string>;
  const event = reject("192.0.2.230:41374", "891e71d7", 1);
  api.agentRejects = (async () => list([event])) as never;
  mount();
  assert.ok(await screen.findByText(zh.refused.replace("{{count}}", "1")));
  assert.ok(screen.getByText(`192.0.2.230 · 凭据 891e71d7… · 1 次 · 首次 ${formatTime(event.at)} · 最近 ${formatTime(event.at)}`));
});

test("an address with no port, IPv6 included, is the host as it stands", () => {
  // Behind a proxy the server reports the forwarded address, which carries no
  // port; a bare IPv6 address must not lose its last group to the port rule.
  const now = Date.parse("2026-11-17T08:40:00Z");
  const groups = rejectedAgents([
    { token_prefix: "aaaaaaaa", reason: "bad_token", remote: "fd00::88", at: "2026-11-17T08:36:00Z" },
    { token_prefix: "aaaaaaaa", reason: "bad_token", remote: "[fd00::88]:49000", at: "2026-11-17T08:31:00Z" },
    { token_prefix: "bbbbbbbb", reason: "bad_token", remote: "10.0.0.7", at: "2026-11-17T08:35:00Z" },
  ], now);
  assert.deepEqual(groups.map((group) => [group.host, group.tokenPrefix, group.count]), [["fd00::88", "aaaaaaaa", 2], ["10.0.0.7", "bbbbbbbb", 1]]);
});

test("the help says what the server saw, the usual causes, and how to remove only the leftover Agent", () => {
  for (const lang of ["en", "zh"]) {
    const rejects = locale(lang, "nodes").rejects as Record<string, string>;
    // The title states the observation; it does not tell the operator to
    // uninstall an Agent that may be the node's own.
    assert.ok(!/uninstall|卸载/i.test(rejects.refused), rejects.refused);
    // Two Agents on one host differ by state directory: a bare uninstall
    // would not say which one is meant.
    assert.ok(rejects.refusedHelp.includes("gpuworks-agent uninstall --state "), rejects.refusedHelp);
  }
});

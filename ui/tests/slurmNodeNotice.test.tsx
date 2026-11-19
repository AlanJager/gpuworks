import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://gpuworks.example/" });
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

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: {
    en: {
      nodes: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/nodes.json", import.meta.url), "utf8")),
      common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")),
    },
  },
  defaultNS: "nodes",
  interpolation: { escapeValue: false },
});

const { cleanup, fireEvent, render, screen } = await import("@testing-library/react");

const { default: SlurmNodeNotice } = await import("../src/components/SlurmNodeNotice.tsx");
const { FeedbackHost, message: zeronMessage } = await import("../src/ui-kit/feedback");

afterEach(() => { cleanup(); zeronMessage.destroy(); });

const renderNotice = (node: Parameters<typeof SlurmNodeNotice>[0]["node"]) =>
  render(<I18nextProvider i18n={i18n}><FeedbackHost /><SlurmNodeNotice node={node} /></I18nextProvider>);

const gresReason = "gres/gpu count reported lower than configured (0 < 2)";

test("nothing is shown while Slurm runs jobs on the node, or on a node outside Slurm", () => {
  for (const node of [{}, { slurm_node: "gpu000" }]) {
    const { container, unmount } = renderNotice(node);
    assert.equal(container.textContent, "");
    assert.equal(screen.queryByRole("alert"), null);
    unmount();
  }
});

const fix = (code: string, ...commands: string[]) => ({ code, commands });
const resume = fix("resume", "scontrol update nodename=gpu000 state=resume");
const gresFixes = [
  fix("gres_conf", "NodeName=gpu000 Name=gpu File=/dev/nvidia[0-1]"),
  fix("reconfigure", "scontrol reconfigure"),
  resume,
  fix("restart_slurmd", "sudo systemctl restart slurmd"),
];

test("a missing gres.conf shows Slurm's words, then the server's steps in order, each command copyable", async () => {
  let copied = "";
  Object.defineProperty(globalThis.navigator, "clipboard", { value: { writeText: async (text: string) => { copied = text; } }, configurable: true });
  renderNotice({ slurm_node: "gpu000", slurm_unavailable: "gres_missing", slurm_reason: gresReason, slurm_fixes: gresFixes });
  const alert = screen.getByRole("alert");
  assert.match(alert.textContent ?? "", /registered node gpu000 without its GPUs/);
  assert.match(alert.textContent ?? "", new RegExp(`Slurm says: ${gresReason.replace(/[()]/g, "\\$&")}`));
  assert.match(alert.textContent ?? "", /add this line to gres.conf/);
  assert.match(alert.textContent ?? "", /restart slurmd on this node/);
  const commands = Array.from(alert.querySelectorAll("code")).map(code => code.textContent);
  assert.deepEqual(commands, gresFixes.flatMap(f => f.commands));
  const buttons = screen.getAllByRole("button", { name: "Copy command" });
  assert.equal(buttons.length, 4);
  fireEvent.click(buttons[0]);
  await screen.findByText("Command copied");
  assert.equal(copied, "NodeName=gpu000 Name=gpu File=/dev/nvidia[0-1]");
});

test("each step's text comes from its code, with the node's name", () => {
  renderNotice({ slurm_node: "gpu000", slurm_unavailable: "drained", slurm_reason: "maintenance", slurm_fixes: [{ ...resume, code: "resume_drained" }] });
  const alert = screen.getByRole("alert");
  assert.match(alert.textContent ?? "", /drained or draining/);
  assert.match(alert.textContent ?? "", /Slurm says: maintenance/);
  assert.match(alert.textContent ?? "", /Once the reason Slurm gives is dealt with, put node gpu000 back in service/);
  assert.deepEqual(Array.from(alert.querySelectorAll("code")).map(code => code.textContent), ["scontrol update nodename=gpu000 state=resume"]);
});

test("the commands are the server's, whatever they are", () => {
  renderNotice({ slurm_node: "gpu000", slurm_unavailable: "down", slurm_fixes: [fix("check_slurmd", "systemctl status slurmd", "sudo journalctl -u slurmd -n 100 --no-pager"), resume] });
  const alert = screen.getByRole("alert");
  assert.deepEqual(Array.from(alert.querySelectorAll("code")).map(c => c.textContent), [
    "systemctl status slurmd",
    "sudo journalctl -u slurmd -n 100 --no-pager",
    "scontrol update nodename=gpu000 state=resume",
  ]);
  assert.doesNotMatch(alert.textContent ?? "", /Slurm says/);
});

test("a code this client has no text for still says what Slurm reported, and no steps without the server's", () => {
  renderNotice({ slurm_node: "gpu000", slurm_unavailable: "something_new", slurm_reason: "odd" });
  const alert = screen.getByRole("alert");
  assert.match(alert.textContent ?? "", /something_new/);
  assert.match(alert.textContent ?? "", /Slurm says: odd/);
  assert.equal(alert.querySelectorAll("code").length, 0);
  assert.equal(alert.querySelectorAll("li").length, 0);
});

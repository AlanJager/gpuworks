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

test("a missing gres.conf shows Slurm's words and the server's line, then the steps in order", async () => {
  let copied = "";
  Object.defineProperty(globalThis.navigator, "clipboard", { value: { writeText: async (text: string) => { copied = text; } }, configurable: true });
  renderNotice({ slurm_node: "gpu000", slurm_unavailable: "gres_missing", slurm_reason: gresReason, slurm_gres_conf: "NodeName=gpu000 Name=gpu File=/dev/nvidia[0-1]" });
  const alert = screen.getByRole("alert");
  assert.match(alert.textContent ?? "", /registered node gpu000 without its GPUs/);
  assert.match(alert.textContent ?? "", new RegExp(`Slurm says: ${gresReason.replace(/[()]/g, "\\$&")}`));
  const commands = Array.from(alert.querySelectorAll("code")).map(code => code.textContent);
  assert.deepEqual(commands, [
    "NodeName=gpu000 Name=gpu File=/dev/nvidia[0-1]",
    "scontrol reconfigure",
    "scontrol update nodename=gpu000 state=resume",
    "sudo systemctl restart slurmd",
  ]);
  // The AutoDetect=nvml route is not offered: not every Slurm build has the plugin.
  assert.doesNotMatch(alert.textContent ?? "", /nvml/i);
  const buttons = screen.getAllByRole("button", { name: "Copy command" });
  assert.equal(buttons.length, 4);
  fireEvent.click(buttons[0]);
  await screen.findByText("Command copied");
  assert.equal(copied, "NodeName=gpu000 Name=gpu File=/dev/nvidia[0-1]");
});

test("a drained node is put back with resume once the cause is dealt with", () => {
  renderNotice({ slurm_node: "gpu000", slurm_unavailable: "drained", slurm_reason: "maintenance" });
  const alert = screen.getByRole("alert");
  assert.match(alert.textContent ?? "", /drained or draining/);
  assert.match(alert.textContent ?? "", /Slurm says: maintenance/);
  assert.deepEqual(Array.from(alert.querySelectorAll("code")).map(code => code.textContent), ["scontrol update nodename=gpu000 state=resume"]);
});

test("a down or refused node is checked on the node first, then resumed", () => {
  for (const code of ["down", "invalid_registration"] as const) {
    const { unmount } = renderNotice({ slurm_node: "gpu000", slurm_unavailable: code });
    const alert = screen.getByRole("alert");
    assert.deepEqual(Array.from(alert.querySelectorAll("code")).map(c => c.textContent), [
      "systemctl status slurmd",
      "sudo journalctl -u slurmd -n 100 --no-pager",
      "scontrol update nodename=gpu000 state=resume",
    ]);
    assert.doesNotMatch(alert.textContent ?? "", /Slurm says/);
    unmount();
  }
});

test("a gres_missing node without a composed line still lists the steps, without that command", () => {
  renderNotice({ slurm_node: "gpu000", slurm_unavailable: "gres_missing" });
  assert.deepEqual(Array.from(screen.getByRole("alert").querySelectorAll("code")).map(c => c.textContent), [
    "scontrol reconfigure",
    "scontrol update nodename=gpu000 state=resume",
    "sudo systemctl restart slurmd",
  ]);
});

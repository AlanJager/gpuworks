import assert from "node:assert/strict";
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
  resources: { en: { nodes: { runtime: {
    pluginMissingHint: "Plugin missing",
    pluginNotInstalled: "not installed",
    copyInstallCommand: "Copy install command",
    pluginInstallCopied: "Copied",
    manualCopyTitle: "Copy command",
  } }, common: { copy: { manualHint: "Copy manually", done: "Done" } } } },
  defaultNS: "nodes",
  interpolation: { escapeValue: false },
});

const { cleanup, fireEvent, render, screen } = await import("@testing-library/react");
const { default: RuntimePluginStatus, runtimePluginInstallCommand, runtimePluginMissing } = await import("../src/components/RuntimePluginStatus.tsx");
const { copyText } = await import("../src/clipboard.ts");

afterEach(() => cleanup());

test("install commands follow the vendor and selected runtime", () => {
  const docker = runtimePluginInstallCommand({ gpu_vendor: "nvidia", runtime_driver: "docker" });
  const nerdctl = runtimePluginInstallCommand({ gpu_vendor: "nvidia", runtime_driver: "nerdctl" });
  const ascend = runtimePluginInstallCommand({ gpu_vendor: "ascend", runtime_driver: "docker" });
  assert.match(docker, /nvidia-container-toolkit/);
  assert.match(docker, /libnvidia-container1-1\.20\.0-1\.x86_64\.rpm/);
  assert.match(docker, /--max-time 6/);
  assert.doesNotMatch(docker, /-r 0-2097151/);
  assert.match(docker, /mirrors\.ustc\.edu\.cn\/libnvidia-container/);
  assert.doesNotMatch(docker, /GPUWORKS_NVIDIA_TOOLKIT_VERSION/);
  assert.match(docker, /runtime configure --runtime=docker/);
  assert.match(docker, /zypper --gpg-auto-import-keys install/);
  assert.doesNotMatch(nerdctl, /runtime configure/);
  assert.match(ascend, /Ascend-docker-runtime/);
  assert.equal(runtimePluginInstallCommand({ gpu_vendor: "ascend", runtime_driver: "nerdctl" }), "");
  assert.equal(runtimePluginInstallCommand({ gpu_vendor: "nvidia", runtime_driver: "udocker" }), "");
});

test("only the server's missing-plugin verdict raises the warning", () => {
  assert.equal(runtimePluginMissing({ gpu_vendor: "nvidia", runtime_driver: "docker", runtime_gpu_blocked: "gpu_plugin_missing", runtime_gpu_plugin: "NVIDIA Container Toolkit" }), true);
  // Installed but not configured is a different cause, with a different fix.
  assert.equal(runtimePluginMissing({ gpu_vendor: "nvidia", runtime_driver: "docker", runtime_gpu_blocked: "gpu_runtime_unconfigured", runtime_plugin: "NVIDIA Container Toolkit 1.20.0" }), false);
  assert.equal(runtimePluginMissing({ gpu_vendor: "nvidia", runtime_driver: "docker", runtime_plugin: "NVIDIA Container Toolkit 1.20.0" }), false);
  assert.equal(runtimePluginMissing({ gpu_vendor: "nvidia", runtime_driver: "docker" }), false);
});

test("an installed plugin shows its name and version, and nothing when the node needs none", () => {
  const { container } = render(<I18nextProvider i18n={i18n}><RuntimePluginStatus node={{ gpu_vendor: "nvidia", runtime_driver: "docker", runtime_plugin: "NVIDIA Container Toolkit 1.20.0", runtime_gpu_plugin: "NVIDIA Container Toolkit" }} /></I18nextProvider>);
  assert.equal(container.textContent, "NVIDIA Container Toolkit 1.20.0");
  assert.equal(screen.queryByRole("alert"), null);
  cleanup();
  const none = render(<I18nextProvider i18n={i18n}><RuntimePluginStatus node={{ gpu_vendor: "apple", runtime_driver: "docker" }} /></I18nextProvider>);
  assert.equal(none.container.textContent, "—");
});

test("a missing plugin is named from runtime_gpu_plugin, not parsed out of the installed version", () => {
  render(<I18nextProvider i18n={i18n}><RuntimePluginStatus node={{ gpu_vendor: "ascend", runtime_driver: "docker", runtime_plugin: "", runtime_gpu_blocked: "gpu_plugin_missing", runtime_gpu_plugin: "Ascend Docker Runtime" }} /></I18nextProvider>);
  assert.equal(screen.getByRole("alert").textContent, "Ascend Docker Runtime · not installed");
  assert.ok(screen.getByRole("button", { name: /Copy install command/ }), "ascend on docker has an install command");
});

test("a missing plugin with no install command for this runtime still warns, without a copy button", () => {
  render(<I18nextProvider i18n={i18n}><RuntimePluginStatus node={{ gpu_vendor: "ascend", runtime_driver: "nerdctl", runtime_gpu_blocked: "gpu_plugin_missing", runtime_gpu_plugin: "Ascend Docker Runtime" }} /></I18nextProvider>);
  assert.match(screen.getByRole("alert").textContent ?? "", /Ascend Docker Runtime/);
  assert.equal(screen.queryByRole("button", { name: /Copy install command/ }), null);
});

test("plain HTTP copy uses the selection fallback", async () => {
  let copied = false;
  Object.defineProperty(globalThis.navigator, "clipboard", { value: undefined, configurable: true });
  Object.defineProperty(globalThis.document, "execCommand", { value: (command: string) => { copied = command === "copy"; return copied; }, configurable: true });
  assert.equal(await copyText("install command"), true);
  assert.equal(copied, true);
  assert.equal(dom.window.document.querySelector("textarea"), null);
});

test("missing plugin renders a warning and exposes manual copy when HTTP fallback fails", async () => {
  Object.defineProperty(globalThis.navigator, "clipboard", { value: { writeText: async () => { throw new Error("insecure context"); } }, configurable: true });
  Object.defineProperty(globalThis.document, "execCommand", { value: () => false, configurable: true });
  assert.equal(await copyText("should fail"), false);
  render(<I18nextProvider i18n={i18n}><RuntimePluginStatus node={{ gpu_vendor: "nvidia", runtime_driver: "docker", runtime_plugin: "", runtime_gpu_blocked: "gpu_plugin_missing", runtime_gpu_plugin: "NVIDIA Container Toolkit" }} /></I18nextProvider>);
  assert.match(screen.getByRole("alert").textContent ?? "", /not installed/);
  fireEvent.click(screen.getByRole("button", { name: /Copy install command/ }));
  const dialog = await screen.findByRole("dialog", { name: "Copy command" });
  assert.ok(dialog.textContent?.includes("Copy manually"));
  const editor = await screen.findByRole("textbox");
  assert.match((editor as HTMLTextAreaElement).value, /nvidia-container-toolkit/);
});

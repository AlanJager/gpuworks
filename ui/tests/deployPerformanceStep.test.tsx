import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

// The KV cache tier switches in the expert performance step set a field that
// has no Form.Item until the tier is on. These cases prove the switches still
// observe the value they set, so the memory tier can be turned on at all and
// the disk tier unlocks behind it.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLTextAreaElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent() { return false; } }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const deployLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deploy.json", import.meta.url), "utf8"));
const commonLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "en", fallbackLng: "en", resources: { en: { deploy: deployLocale, common: commonLocale } }, defaultNS: "common", interpolation: { escapeValue: false } });
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { Form } = await import("antd");
type FormInstance = import("antd").FormInstance;
const DeployPerformanceStep = (await import("../src/pages/deploy/DeployPerformanceStep.tsx")).default;

// The dictionary rows the server publishes for the vLLM KV tiers, spelled the
// way /engines emits them (internal/server/deploy/param_specs.go).
const utilizationSpec = { key: "gpu_memory_utilization", flag: "--gpu-memory-utilization", type: "number", control: "slider", group: "performance", default: 0.9, min: 0.7, max: 0.98, step: 0.01, label: { en: "GPU memory utilization", zh: "显存利用率" } } as never;
const memoryTierSpec = { key: "kv_offloading_size", flag: "--kv-offloading-size", type: "number", control: "tier", group: "performance", min: 1, step: 1, label: { en: "CPU KV offload size", zh: "CPU KV 卸载大小" } } as never;
const diskTierSpec = { key: "kv_disk_offload", flag: "--kv-transfer-config", type: "boolean", control: "tier", group: "performance", default: false, depends_on: "kv_offloading_size", label: { en: "Disk KV offload", zh: "磁盘 KV 卸载" } } as never;
const sharedDirSpec = { key: "shared_kv_dir", flag: "--kv-transfer-config", type: "string", control: "path", group: "performance", depends_on: "kv_disk_offload", label: { en: "Shared KV directory", zh: "共享 KV 目录" } } as never;
const readThreadsSpec = { key: "kv_offload_read_threads", flag: "--kv-transfer-config", type: "integer", control: "number", group: "performance", default: 16, min: 1, step: 1, depends_on: "kv_disk_offload", label: { en: "Read threads", zh: "读线程" } } as never;
const writeThreadsSpec = { key: "kv_offload_write_threads", flag: "--kv-transfer-config", type: "integer", control: "number", group: "performance", default: 16, min: 1, step: 1, depends_on: "kv_disk_offload", label: { en: "Write threads", zh: "写线程" } } as never;
const diskLimitSpec = { key: "kv_disk_limit_gib", flag: "", type: "number", control: "number", group: "performance", automatic: true, min: 1, step: 1, depends_on: "kv_disk_offload", label: { en: "Disk cache limit", zh: "磁盘缓存上限" } } as never;
const profile = { name: "vllm", sleep: { supported: true }, param_specs: [utilizationSpec, memoryTierSpec, diskTierSpec, sharedDirSpec, readThreadsSpec, writeThreadsSpec, diskLimitSpec] } as never;
const model = { id: 1, name: "org/qwen", source_id: "org/qwen", input_modalities: ["text"] } as never;

afterEach(cleanup);
after(() => { closeChannels(); dom.window.close(); });

// Absence is asserted through match counts, never by comparing a queried node
// against null: a rendered element carries React's fiber tree on itself, and an
// assertion that fails while holding one makes the runner's diff walk that
// whole graph until the process is killed.
const fieldCount = (label: string) => screen.queryAllByLabelText(label).length;

function performanceStep(capture: (form: FormInstance) => void, initialValues: Record<string, unknown> = {}) {
  function Host() {
    const [form] = Form.useForm();
    capture(form);
    return <I18nextProvider i18n={i18n}><Form form={form} layout="vertical" initialValues={initialValues}><DeployPerformanceStep profile={profile} engine="vllm" model={model} /></Form></I18nextProvider>;
  }
  return render(<Host />);
}

test("the memory tier switch turns the tier on and exposes its size field", async () => {
  let form!: FormInstance;
  performanceStep((instance) => { form = instance; });
  const [memorySwitch, diskSwitch] = screen.getAllByRole("switch");
  assert.equal(memorySwitch.getAttribute("aria-checked"), "false");
  assert.equal(diskSwitch.getAttribute("aria-checked"), "false");
  assert.ok(diskSwitch.hasAttribute("disabled"), "the disk tier is locked until memory is on");
  assert.equal(fieldCount("CPU KV offload size"), 0);

	await userEvent.click(memorySwitch);

	await waitFor(() => assert.equal(memorySwitch.getAttribute("aria-checked"), "true"));
	assert.equal(form.getFieldValue("kv_offloading_size"), undefined, "opening a new draft must not invent an 8 GiB capacity");
	assert.ok(diskSwitch.hasAttribute("disabled"), "intent alone does not make an empty capacity effective");
	const capacity = screen.getByLabelText("CPU KV offload size");
	await assert.rejects(form.validateFields(["kv_offloading_size"]));
	await userEvent.type(capacity, "32");
	assert.equal(form.getFieldValue("kv_offloading_size"), 32);
	await waitFor(() => assert.ok(!diskSwitch.hasAttribute("disabled"), "the disk tier unlocks once memory is on"));
});

test("the disk tier switch turns on behind the memory tier and exposes its companions", async () => {
  let form!: FormInstance;
  performanceStep((instance) => { form = instance; }, { kv_offloading_size: 16 });
  const [memorySwitch, diskSwitch] = screen.getAllByRole("switch");
  assert.equal(memorySwitch.getAttribute("aria-checked"), "true");
  assert.equal(fieldCount("Shared KV directory"), 0);

  await userEvent.click(diskSwitch);

  await waitFor(() => assert.equal(diskSwitch.getAttribute("aria-checked"), "true"));
	assert.equal(form.getFieldValue("kv_disk_offload"), true);
	assert.ok(screen.getByLabelText("Shared KV directory"));
	assert.ok(screen.getByLabelText("Read threads"));
	assert.ok(screen.getByLabelText("Write threads"));
	assert.ok(screen.getByLabelText("Disk cache limit"));
});

test("turning the memory tier off preserves the whole disk draft and locks its controls", async () => {
	let form!: FormInstance;
	performanceStep((instance) => { form = instance; }, {
		kv_offloading_size: 16,
		kv_disk_offload: true,
		shared_kv_dir: "/mnt/custom-kv",
		kv_offload_read_threads: 4,
		kv_offload_write_threads: 6,
		kv_disk_limit_gib: 96,
	});
	const [memorySwitch, diskSwitch] = screen.getAllByRole("switch");
	await userEvent.click(memorySwitch);
	await waitFor(() => assert.equal(memorySwitch.getAttribute("aria-checked"), "false"));
	assert.equal(form.getFieldValue("kv_offloading_size"), 16);
	assert.equal(form.getFieldValue("kv_disk_offload"), true);
	assert.equal(form.getFieldValue("shared_kv_dir"), "/mnt/custom-kv");
	assert.equal(form.getFieldValue("kv_offload_read_threads"), 4);
	assert.equal(form.getFieldValue("kv_offload_write_threads"), 6);
	assert.equal(form.getFieldValue("kv_disk_limit_gib"), 96);
	await waitFor(() => assert.equal(diskSwitch.getAttribute("aria-checked"), "true"));
	assert.equal(fieldCount("CPU KV offload size"), 0);
	assert.equal(fieldCount("Shared KV directory"), 0);
	await waitFor(() => assert.ok(diskSwitch.hasAttribute("disabled")));

	await userEvent.click(memorySwitch);
	await waitFor(() => assert.equal(fieldCount("Shared KV directory"), 1));
	assert.equal((screen.getByLabelText("CPU KV offload size") as HTMLInputElement).value, "16");
	assert.equal((screen.getByLabelText("Shared KV directory") as HTMLInputElement).value, "/mnt/custom-kv");
	assert.equal((screen.getByLabelText("Disk cache limit") as HTMLInputElement).value, "96");
});

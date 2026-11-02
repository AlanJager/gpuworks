import { dom, createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import type { EngineProfile, Node, PartitionCapability } from "../src/api.ts";

const { cleanup, fireEvent, render, screen } = await import("@testing-library/react");
const { Form } = await import("antd");
const { api } = await import("../src/api.ts");
const { installFleet } = await import("./helpers/fakeFleet.ts");
const ExpertTabs = (await import("../src/pages/deploy/DeployExpertTabs.tsx")).default;
const { i18n, locale } = await createI18n(["deploy"]);
const profiles = JSON.parse(fs.readFileSync(new URL("./fixtures/engines.json", import.meta.url), "utf8")) as EngineProfile[];
// Retain actual published metadata for the only two dictionary rows needed
// by this reproduction; unrelated text/numeric/LoRA controls are not involved.
const profileFor = (engine: string) => {
  const profile = profiles.find(row => row.name === engine)!;
  assert.ok(profile);
  return { ...profile, param_specs: profile.param_specs?.filter(spec =>
    ["kv_offloading_size", "kv_disk_offload"].includes(spec.key)) };
};
const capability = { backend: "nvidia-dgpu", vendor: "nvidia", status: "ready",
  display_name: "NVIDIA dGPU", maturity: "stable" } as PartitionCapability;
const node = { uuid: "owned-node", name: "owned-A100", state: "online", gpu_count: 1,
  gpus: [{ index: 0, uuid: "GPU-FAKE-0", name: "A100", vendor: "nvidia",
    memory_total_bytes: 24576 * 2 ** 20, partition_capabilities: [capability] }] } as Node;
const originalApi = { ...api };
let form: import("antd").FormInstance;
function Host({ profile, values }: { profile: EngineProfile; values: Record<string, unknown> }) {
  [form] = Form.useForm();
  return <Form form={form} initialValues={{ engine: profile.name, node_selection_mode: "auto", nnodes: 1, replicas: 1, ...values }}>
    {/* The real parent receives the same profile as Quick; exercise its real
        ResourceStep wiring instead of adding invented profile props to it. */}
    <ExpertTabs engineProfile={profile} selectedEngine={profile.name}
      selectedNode={node} selectedPartitionCapability={capability}
      engines={{ data: [profile], loading: false }} images={[]} imageMode="custom"
      imageSeriesLoading={false} inferenceSeries={[]} autoResolutionText="" evalResult={null} />
  </Form>;
}
function mount(engine: string, tab: "resource" | "performance", values: Record<string, unknown> = {}) {
  installFleet(api as never, [node]);
  render(<I18nextProvider i18n={i18n}><Host profile={profileFor(engine)} values={values} /></I18nextProvider>);
  fireEvent.click(screen.getByRole("tab", { name: locale.deploy.page.step[tab], exact: true }));
}
function nativeSwitch(name: string): HTMLButtonElement {
  const element = screen.getByRole("switch", { name, exact: true });
  assert.ok(element instanceof dom.window.HTMLButtonElement, "observe the actual native Switch button");
  return element as HTMLButtonElement;
}
afterEach(() => { cleanup(); Object.assign(api, originalApi); });
after(() => dom.window.close());

test("Expert Resource disables sleep for the actual unsupported SGLang profile", () => {
  assert.equal(profileFor("sglang").sleep.supported, false);
  mount("sglang", "resource");
  const sleep = nativeSwitch(locale.deploy.resource.sleepable);
  assert.equal(sleep.disabled, true, "unsupported SGLang sleep must be natively disabled like Quick");
  assert.ok(screen.queryByText(locale.deploy.workbench.engineUnsupported, { exact: true }),
    "the unsupported switch must show the exact existing unsupported reason");
});

test("Expert Resource keeps supported vLLM sleep enabled and writes the real Form", () => {
  assert.equal(profileFor("vllm").sleep.supported, true);
  mount("vllm", "resource");
  const sleep = nativeSwitch(locale.deploy.resource.sleepable);
  assert.equal(sleep.disabled, false);
  fireEvent.click(sleep);
  assert.equal(form.getFieldValue("sleepable"), true);
  assert.equal(sleep.getAttribute("aria-checked"), "true");
  assert.equal(screen.queryByText(locale.deploy.workbench.engineUnsupported, { exact: true }), null);
});

test("Expert disk offload explains its actual disabled memory prerequisite", () => {
  mount("vllm", "performance", { kv_offloading_size: 8, kv_memory_tier_enabled: false });
  assert.equal(nativeSwitch(locale.deploy.performance.cpuTier).getAttribute("aria-checked"), "false");
  assert.equal(nativeSwitch(locale.deploy.performance.diskTier).disabled, true);
  assert.ok(screen.queryByText(locale.deploy.workbench.kv.memoryRequired, { exact: true }),
    "disabled disk offload must state Enable the memory tier first, not only generic transport help");
});

test("enabling the real CPU tier unlocks disk offload and removes the disabled-only reason", () => {
  mount("vllm", "performance", { kv_offloading_size: 8, kv_memory_tier_enabled: false });
  const cpu = nativeSwitch(locale.deploy.performance.cpuTier);
  const disk = nativeSwitch(locale.deploy.performance.diskTier);
  assert.equal(disk.disabled, true);
  fireEvent.click(cpu);
  assert.equal(form.getFieldValue("kv_memory_tier_enabled"), true);
  assert.equal(disk.disabled, false);
  fireEvent.click(disk);
  assert.equal(form.getFieldValue("kv_disk_offload"), true);
  assert.equal(disk.getAttribute("aria-checked"), "true");
  assert.ok(screen.queryByText(locale.deploy.performance.diskTierHelp, { exact: true }),
    "the enabled disk tier retains its original transport help");
  assert.equal(screen.queryByText(locale.deploy.workbench.kv.memoryRequired, { exact: true }), null);
});

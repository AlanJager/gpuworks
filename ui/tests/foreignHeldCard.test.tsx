import { dom, createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import type { GPU, GPUCandidates, Node as GPUNode } from "../src/api.ts";

const { render, screen, cleanup } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { Form } = await import("antd");
const { GPUGrid } = await import("../src/pages/deploy/DeployChoiceCards.tsx");
const { scheduleText } = await import("../src/pages/deploy/deploySchedule.ts");
const { i18n } = await createI18n(["deploy", "nodes"]);
const t = i18n.getFixedT("en", "deploy");
afterEach(() => cleanup());
after(() => dom.window.close());

// The node of flitai-io/gpuworks#20: GPU 0 of an H20-3e pair is held by a
// GPUStack container outside the platform, so the inventory says it takes no
// work; GPU 1 is free.
const held = {
  index: 0, uuid: "gpu-0", name: "NVIDIA H20-3e", memory_total_bytes: 143771 * 1024 ** 2, schedulable: false, health_status: "OK",
  foreign_processes: [{ pid: 41235, name: "python3", memory_bytes: 67089334272 }],
} as GPU;
const free = { index: 1, uuid: "gpu-1", name: "NVIDIA H20-3e", memory_total_bytes: 143771 * 1024 ** 2, schedulable: true, health_status: "OK" } as GPU;
const node = { uuid: "h20", name: "h20", state: "online", gpu_count: 2, gpus: [held, free] } as GPUNode;

let form: ReturnType<typeof Form.useForm>[0];
function Grid({ value }: { value?: GPUCandidates }) {
  [form] = Form.useForm();
  return <I18nextProvider i18n={i18n}><Form form={form} initialValues={{ node_selection_mode: "manual", gpu_indexes: [] }}>
    <GPUGrid node={node} resourceContext={{ nodeUUID: node.uuid, candidates: value, t }} />
  </Form></I18nextProvider>;
}

test("a card held outside the platform can be picked for a partition the server says fits beside it", async () => {
  const partition: GPUCandidates = { node_uuid: node.uuid, items: [{ index: 0, available: true }, { index: 1, available: true }] };
  render(<Grid value={partition} />);
  const card = screen.getByRole("button", { name: "0" }) as HTMLButtonElement;
  assert.equal(card.disabled, false);
  assert.ok(!card.classList.contains("is-unavailable"));
  card.focus();
  await userEvent.setup({ document: dom.window.document }).keyboard(" ");
  assert.deepEqual(form.getFieldValue("gpu_indexes"), [0]);
});

test("a whole-card request leaves the held card unpickable, naming why", () => {
  const whole: GPUCandidates = { node_uuid: node.uuid, items: [
    { index: 0, available: false, reason: { code: "gpus_held_outside_platform" } }, { index: 1, available: true },
  ] };
  render(<Grid value={whole} />);
  const card = screen.getByRole("button", { name: "0" }) as HTMLButtonElement;
  assert.equal(card.disabled, true);
  assert.ok(card.classList.contains("is-unavailable"));
});

test("before the server has evaluated the request the held card is not offered", () => {
  render(<Grid />);
  assert.equal((screen.getByRole("button", { name: "0" }) as HTMLButtonElement).disabled, true);
  assert.equal((screen.getByRole("button", { name: "1" }) as HTMLButtonElement).disabled, false);
});

test("a no_fit reason names what processes outside the platform hold, in both languages", async () => {
  const params = { gpus: 1, tp: 1, pp: 1, gpu_count: 1, required_gib: "50.5", available_gib: "20.4", gpu_memory_utilization: 0.9,
    foreign_gib: "62.5", foreign_headroom_gib: "7.0" };
  const text = scheduleText(t, { code: "no_fit", params });
  assert.match(text, /20\.4 GiB available/);
  assert.match(text, /outside the platform use 62\.5 GiB of the card, and 7\.0 GiB is kept free/);
  const { i18n: zh } = await createI18n(["deploy"], "zh");
  assert.match(scheduleText(zh.getFixedT("zh", "deploy"), { code: "no_fit", params }), /平台外的进程占用此卡 62\.5 GiB，另在其旁保留 7\.0 GiB 余量/);
  assert.doesNotMatch(scheduleText(t, { code: "no_fit", params: { ...params, foreign_gib: undefined } }), /outside the platform/);
  // The deployment policy keeps no headroom: the server states none, and
  // the text invents none.
  const bare = { ...params, foreign_headroom_gib: undefined };
  assert.match(scheduleText(t, { code: "no_fit", params: bare }), /outside the platform use 62\.5 GiB of the card\.$/);
  assert.doesNotMatch(scheduleText(t, { code: "no_fit", params: bare }), /kept free|undefined/);
  assert.match(scheduleText(zh.getFixedT("zh", "deploy"), { code: "no_fit", params: bare }), /平台外的进程占用此卡 62\.5 GiB。$/);
});

test("a sleepable partition is told why the shared card is refused", () => {
  assert.match(scheduleText(t, { code: "held_outside_platform_sleep" }), /could not be woken there/);
});

test("an offered card shared with processes outside the platform says so when picked", () => {
  const partition: GPUCandidates = { node_uuid: node.uuid, items: [
    { index: 0, available: true, note: { code: "foreign_shared_card", params: { foreign_gib: "62.5" } } }, { index: 1, available: true },
  ] };
  render(<Grid value={partition} />);
  const shared = screen.getByRole("button", { name: "0" }) as HTMLButtonElement;
  assert.equal(shared.disabled, false);
  assert.match(shared.title, /Shared with processes outside the platform \(62\.5 GiB\); compute is not isolated/);
  assert.doesNotMatch((screen.getByRole("button", { name: "1" }) as HTMLButtonElement).title, /outside the platform/);
});

test("a refusal over cards nobody enabled names the cards and where to enable them", async () => {
  const params = { gpus: [0, 1], count: 2, node: "h20" };
  const en = scheduleText(t, { code: "held_outside_platform_not_allowed", params });
  assert.match(en, /GPU 0, 1 on this node are used by processes outside the platform and scheduling is not enabled on them/);
  assert.match(en, /Enable scheduling on them in the node's GPU tab, or pick the cards yourself/);
  assert.doesNotMatch(en, /placement policy|\{\{/);
  const { i18n: zh } = await createI18n(["deploy", "nodes"], "zh");
  const text = scheduleText(zh.getFixedT("zh", "deploy"), { code: "held_outside_platform_not_allowed", params });
  assert.match(text, /GPU 0, 1 有平台外进程，未启用调度。请在节点的 GPU 页签对它们启用调度/);
  assert.doesNotMatch(text, /放置策略|\{\{/);
});

test("a plan on a shared card states how many and how much", () => {
  assert.match(scheduleText(t, { code: "foreign_shared_gpus_used", params: { gpus: [0], count: 1, foreign_gib: "62.5" } }),
    /1 of the chosen GPUs are shared with processes outside the platform \(up to 62\.5 GiB\)/);
});

import assert from "node:assert/strict";
import { after, test } from "node:test";
import "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// Which orders a deployment's schedule offers: sleep and wake need a sleepable
// deployment; a replica count needs a non-PD deployment whose new replicas get
// their nodes picked - single-node, or multi-node placed automatically.

const closeChannels = trackMessageChannels();
const { scheduleActions } = await import("../src/pages/DeploymentSchedule.tsx");
type Dep = Parameters<typeof scheduleActions>[0];
after(() => closeChannels());

const dep = (fields: Record<string, unknown>) => fields as unknown as Dep;

test("a single-node deployment can be scaled on a schedule, and slept when sleepable", () => {
  assert.deepEqual(scheduleActions(dep({ nnodes: 1 })), ["replicas"]);
  assert.deepEqual(scheduleActions(dep({ nnodes: 1, sleepable: true })), ["sleep", "wake", "replicas"]);
  assert.deepEqual(scheduleActions(dep({})), ["replicas"]);
});

test("a multi-node deployment can be scaled only when the platform places it", () => {
  assert.deepEqual(scheduleActions(dep({ nnodes: 2, placement_mode: "auto" })), ["replicas"]);
  assert.deepEqual(scheduleActions(dep({ nnodes: 2, placement_mode: "manual" })), []);
  assert.deepEqual(scheduleActions(dep({ nnodes: 2, sleepable: true })), ["sleep", "wake"]);
});

test("a PD deployment is never scaled on a schedule, even placed automatically", () => {
  assert.deepEqual(scheduleActions(dep({ pd: { mode: "vllm-nixl" }, placement_mode: "auto", sleepable: true })), ["sleep", "wake"]);
});

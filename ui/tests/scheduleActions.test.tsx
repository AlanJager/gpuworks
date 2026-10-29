import assert from "node:assert/strict";
import { after, test } from "node:test";
import "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// Which orders a deployment's schedule offers: sleep and wake need a sleepable
// deployment; a replica count needs a deployment whose new replicas get their
// nodes picked - single-node, or multi-node or PD placed automatically.

const closeChannels = trackMessageChannels();
const { scheduleActions, scheduleRoles } = await import("../src/pages/DeploymentSchedule.tsx");
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

test("a PD deployment's role counts are scheduled only when the platform places it", () => {
  assert.deepEqual(scheduleActions(dep({ pd: { mode: "vllm-nixl" }, placement_mode: "auto", sleepable: true })), ["sleep", "wake", "replicas"]);
  assert.deepEqual(scheduleActions(dep({ pd: { mode: "vllm-nixl" }, placement_mode: "manual", sleepable: true })), ["sleep", "wake"]);
  // A PD deployment reports no single-node shape that would stand in for auto.
  assert.deepEqual(scheduleActions(dep({ pd: { mode: "vllm-nixl" }, nnodes: 1 })), []);
});

test("a PD deployment's roles come with the fewest replicas each may be given", () => {
  assert.deepEqual(
    scheduleRoles(dep({ pd: { roles: [{ role: "decode", desired: 2 }, { role: "prefill", desired: 1, may_be_empty: true }] } })),
    [{ role: "decode", minimum: 1, desired: 2 }, { role: "prefill", minimum: 0, desired: 1 }],
  );
  assert.deepEqual(scheduleRoles(dep({})), []);
});

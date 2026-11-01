import assert from "node:assert/strict";
import { after, test } from "node:test";
import "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import * as shapes from "./helpers/deploymentCapabilities.ts";

// Which orders a deployment's schedule offers is the server's answer
// (schedule_capabilities): the page reads it, whatever sleepable, nnodes or pd
// say, and takes each replica count's bounds from it.

const closeChannels = trackMessageChannels();
const { scheduleActions, scheduleRefusals, scheduleRoles } = await import("../src/pages/DeploymentSchedule.tsx");
type Dep = Parameters<typeof scheduleActions>[0];
after(() => closeChannels());

const dep = (fields: Record<string, unknown>) => ({ replicas: 1, ...fields }) as unknown as Dep;

test("the orders offered are the ones the server allows", () => {
  assert.deepEqual(scheduleActions(dep(shapes.sleepableSingleNode)), ["sleep", "wake", "replicas"]);
  assert.deepEqual(scheduleActions(dep(shapes.notSleepableSingleNode)), ["replicas"]);
  assert.deepEqual(scheduleActions(dep(shapes.sleepableMultiNodeManual)), ["sleep", "wake"]);
  assert.deepEqual(scheduleActions(dep(shapes.sleepableMultiNodeAuto)), ["sleep", "wake", "replicas"]);
  assert.deepEqual(scheduleActions(dep(shapes.notSleepableMultiNodeManual)), []);
  assert.deepEqual(scheduleActions(dep(shapes.sleepablePDAuto)), ["sleep", "wake", "replicas"]);
  assert.deepEqual(scheduleActions(dep(shapes.sleepablePDManual)), ["sleep", "wake"]);
});

test("the server decides even where the deployment's own fields say otherwise", () => {
  // Sleepable, single-node, yet the server refuses sleep (its engine cannot):
  // the page follows the server.
  const refused = { ok: false, reason: "SLEEP_ENGINE_UNSUPPORTED" };
  const engineCannotSleep = dep({
    sleepable: true, nnodes: 1,
    schedule_capabilities: { sleep: refused, wake: refused, replicas: [{ minimum: 1, maximum: 32, ok: true }] },
  });
  assert.deepEqual(scheduleActions(engineCannotSleep), ["replicas"]);
  assert.deepEqual(scheduleRefusals(engineCannotSleep), ["SLEEP_ENGINE_UNSUPPORTED"]);
  // With no capabilities the page offers nothing rather than guessing.
  assert.deepEqual(scheduleActions(dep({ sleepable: true, nnodes: 1 })), []);
});

test("the refusals are the server's codes, each once", () => {
  assert.deepEqual(scheduleRefusals(dep(shapes.notSleepableMultiNodeManual)), ["SLEEP_NOT_ENABLED", "SCHEDULE_REPLICAS_MANUAL_PLACEMENT"]);
  assert.deepEqual(scheduleRefusals(dep(shapes.notSleepablePDManual)), ["SLEEP_NOT_ENABLED", "SCHEDULE_REPLICAS_MANUAL_PLACEMENT"]);
  assert.deepEqual(scheduleRefusals(dep(shapes.sleepableSingleNode)), []);
});

test("each replica count comes with the server's bounds: a PD role's own, in the server's order", () => {
  const pd = dep({ ...shapes.sleepablePDAuto, pd: { roles: [{ role: "prefill", desired: 1 }, { role: "decode", desired: 2 }] } });
  assert.deepEqual(scheduleRoles(pd), [
    { role: "decode", minimum: 1, maximum: 32, desired: 2 },
    { role: "prefill", minimum: 0, maximum: 32, desired: 1 },
  ]);
  const single = dep({ ...shapes.sleepableSingleNode, replicas: 3, schedule_capabilities: { ...shapes.sleepableSingleNode.schedule_capabilities, replicas: [{ minimum: 1, maximum: 8, ok: true }] } });
  assert.deepEqual(scheduleRoles(single), [{ role: "", minimum: 1, maximum: 8, desired: 3 }]);
  // A count the server refuses is not offered.
  assert.deepEqual(scheduleRoles(dep(shapes.sleepablePDManual)), []);
});

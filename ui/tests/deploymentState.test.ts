import assert from "node:assert/strict";
import test from "node:test";

import { effectiveDeploymentState, partlyAsleep, stateLabel } from "../src/pages/state.ts";

test("deployment status uses the derived runtime state across list and detail views", () => {
	const deployment = { desired_state: "active", runtime_state: "running" };
  const t = (key: string) => ({ "states.running": "Running" })[key] ?? key;

  assert.equal(effectiveDeploymentState(deployment), "running");
  assert.deepEqual(stateLabel(effectiveDeploymentState(deployment), t), { status: "success", text: "Running" });
});

test("a failed runtime shows as failed", () => {
	assert.equal(effectiveDeploymentState({ runtime_state: "failed" }), "failed");
});

test("a delete in flight shows as deleting whatever the runtime reports", () => {
	const t = (key: string) => ({ "states.deleting": "Deleting", "states.running": "Running", "states.stopped": "Stopped" })[key] ?? key;

	for (const runtime_state of ["running", "starting", "stopped", "failed"]) {
		const deployment = { desired_state: "deleting", runtime_state };
		assert.equal(effectiveDeploymentState(deployment), "deleting");
		assert.deepEqual(stateLabel(effectiveDeploymentState(deployment), t), { status: "processing", text: "Deleting" });
	}
});

test("without a pending delete the runtime state wins", () => {
	assert.equal(effectiveDeploymentState({ desired_state: "active", runtime_state: "running" }), "running");
	assert.equal(effectiveDeploymentState({ desired_state: "stopped", runtime_state: "stopped" }), "stopped");
	assert.equal(effectiveDeploymentState({ runtime_state: "running" }), "running");
});

test("a sleep order shows as sleeping or asleep, never as plain running", () => {
	const t = (key: string) =>
		({ "states.running": "Running", "states.sleeping": "Sleeping", "states.asleep": "Asleep" })[key] ?? key;

	// The engines still report running while they sleep, so the server's state
	// is what the badge follows.
	for (const [state, want] of [
		["running", { status: "success", text: "Running" }],
		["sleeping", { status: "processing", text: "Sleeping" }],
		["asleep", { status: "warning", text: "Asleep" }],
	] as const) {
		const deployment = { desired_state: "active", runtime_state: "running", state };
		assert.equal(effectiveDeploymentState(deployment), state);
		assert.deepEqual(stateLabel(effectiveDeploymentState(deployment), t), want);
	}
});

test("a delete in flight outranks a sleep order", () => {
	const deployment = { desired_state: "deleting", runtime_state: "running", state: "asleep" };
	assert.equal(effectiveDeploymentState(deployment), "deleting");
});

test("a partly slept deployment still serves, so only its count marks it", () => {
	const partial = { desired_state: "active", runtime_state: "running", state: "running", asleep_replicas: 1, replicas: 4 };
	assert.equal(effectiveDeploymentState(partial), "running");
	assert.equal(partlyAsleep(partial), true);
	// Entirely asleep is no longer "partly", and an awake deployment is not at
	// all — a count of zero must not mark every row.
	assert.equal(partlyAsleep({ ...partial, state: "asleep", asleep_replicas: 4 }), false);
	assert.equal(partlyAsleep({ desired_state: "active", runtime_state: "running", state: "running", asleep_replicas: 0 }), false);
});

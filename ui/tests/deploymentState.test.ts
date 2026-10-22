import assert from "node:assert/strict";
import test from "node:test";

import { effectiveDeploymentState, stateLabel } from "../src/pages/state.ts";

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

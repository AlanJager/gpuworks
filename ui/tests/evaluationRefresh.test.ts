import assert from "node:assert/strict";
import test from "node:test";
import { createRefreshCoalescer } from "../src/pages/evaluations/evaluationRefresh.ts";

const tick = () => new Promise((resolve) => setTimeout(resolve, 5));

test("event refresh coalescer avoids overlap and performs one trailing refresh", async () => {
	let calls = 0, active = 0, maxActive = 0;
	let release!: () => void;
	const first = new Promise<void>((resolve) => { release = resolve; });
	const coalescer = createRefreshCoalescer(async () => {
		calls += 1;
		active += 1;
		maxActive = Math.max(maxActive, active);
		if (calls === 1) await first;
		active -= 1;
	}, 0);
	coalescer.trigger();
	await tick();
	coalescer.trigger();
	coalescer.trigger();
	release();
	await tick();
	await tick();
	coalescer.dispose();
	assert.equal(calls, 2);
	assert.equal(maxActive, 1);
});

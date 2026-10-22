import assert from "node:assert/strict";
import { test } from "node:test";

import { DEFAULT_DEPLOY_SCENARIOS, deployScenarioLabel } from "../src/pages/deploy/deployScenario.ts";

const labels: Record<string, string> = {
  "quick.scenarioValue.latency": "Low Latency",
  "quick.scenarioValue.balanced": "Balanced",
  "quick.scenarioValue.throughput": "High Throughput",
  "quick.scenarioValue.unspecified": "Unspecified",
};
const t = ((key: string) => labels[key] ?? key) as never;

test("deploy scenarios translate persisted catalog values and stable aliases", () => {
  assert.equal(deployScenarioLabel(t, DEFAULT_DEPLOY_SCENARIOS[0]), "Low Latency");
  assert.equal(deployScenarioLabel(t, DEFAULT_DEPLOY_SCENARIOS[1]), "Balanced");
  assert.equal(deployScenarioLabel(t, DEFAULT_DEPLOY_SCENARIOS[2]), "High Throughput");
  assert.equal(deployScenarioLabel(t, "interactivity"), "Low Latency");
  assert.equal(deployScenarioLabel(t, "balanced"), "Balanced");
  assert.equal(deployScenarioLabel(t, "high_throughput"), "High Throughput");
});

test("deploy scenarios preserve unknown external values", () => {
  assert.equal(deployScenarioLabel(t, "custom-scenario"), "custom-scenario");
  assert.equal(deployScenarioLabel(t, ""), "Unspecified");
});

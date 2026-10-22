import assert from "node:assert/strict";
import { test } from "node:test";

import { warningKey, warningText } from "../src/pages/deploy/deployWarnings.ts";

// Every warning rendered here is produced by the server and arrives as a stable
// code plus parameters. Since ADR 0020 the front end derives none of its own —
// deployHelpers.test.ts holds that assertion — so these cases cover rendering
// only: interpolation, a stable key, and locale-aware parameters.

test("warning rendering forwards params and falls back to the code", () => {
  const calls: Array<{ key: string; options: Record<string, unknown> }> = [];
  const t = ((key: string, options: Record<string, unknown>) => {
    calls.push({ key, options });
    return `${key}:${options.required_cores}`;
  }) as never;
  const warning = { code: "W10", params: { cpu_cores: 2, required_cores: 4 } };
  assert.equal(warningText(t, warning), "warning.W10:4");
  assert.deepEqual(calls, [{ key: "warning.W10", options: { cpu_cores: 2, required_cores: 4, defaultValue: "W10" } }]);
});

test("warning keys are stable across parameter insertion order", () => {
  assert.equal(
    warningKey({ code: "W1", params: { max_num_seqs: 2, max_model_len: 8 } }),
    warningKey({ code: "W1", params: { max_model_len: 8, max_num_seqs: 2 } }),
  );
});

test("PD_UNVERIFIED_COMBINATION renders its feature through the translated map", () => {
  const t = ((key: string, options?: Record<string, unknown>) => {
    if (key === "warningFeature.dgpu") return "dynamic GPU (dGPU)";
    if (key === "warning.PD_UNVERIFIED_COMBINATION") return `feature=${options?.feature}`;
    return key;
  }) as never;
  assert.equal(
    warningText(t, { code: "PD_UNVERIFIED_COMBINATION", params: { feature: "dgpu" } }),
    "feature=dynamic GPU (dGPU)",
  );
});

test("PD_UNVERIFIED_COMBINATION falls back to the raw feature value when the map does not know it", () => {
  const t = ((key: string, options?: Record<string, unknown>) => {
    if (key === "warningFeature.a_future_feature") return options?.defaultValue as string;
    if (key === "warning.PD_UNVERIFIED_COMBINATION") return `feature=${options?.feature}`;
    return key;
  }) as never;
  assert.equal(
    warningText(t, { code: "PD_UNVERIFIED_COMBINATION", params: { feature: "a_future_feature" } }),
    "feature=a_future_feature",
  );
});

test("warning rendering localizes persisted inference-template scenario parameters", () => {
  const t = ((key: string, options?: Record<string, unknown>) => {
    if (key === "quick.scenarioValue.latency") return "Low Latency";
    if (key === "warning.INFERENCE_TEMPLATE_SCENARIO_FIXTURE") return `scenario=${options?.scenario}`;
    return key;
  }) as never;
  assert.equal(warningText(t, {
    code: "INFERENCE_TEMPLATE_SCENARIO_FIXTURE",
    params: { scenario: String.fromCodePoint(0x4f4e, 0x5ef6, 0x8fdf) },
  }), "scenario=Low Latency");
});

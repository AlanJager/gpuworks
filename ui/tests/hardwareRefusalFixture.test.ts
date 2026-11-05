import assert from "node:assert/strict";
import fs from "node:fs";
import { test } from "node:test";
import { createInstance } from "i18next";
import type { EvaluateResponse, ScheduleMessage } from "../src/api.ts";
import { placementRefusalText } from "../src/pages/deploy/deploySchedule.ts";

const read = (path: string) => JSON.parse(fs.readFileSync(new URL(path, import.meta.url), "utf8"));
const amd = read("./fixtures/evaluate/amd_chip_mismatch_reason.json") as ScheduleMessage;
const nvidia = read("./fixtures/evaluate/template_vllm.json") as EvaluateResponse;

for (const lng of ["en", "zh"]) {
  const i18n = createInstance();
  await i18n.init({ lng, resources: { [lng]: { deploy: read(`../src/i18n/locales/${lng}/deploy.json`) } }, defaultNS: "deploy", initImmediate: false, interpolation: { escapeValue: false } });
  const t = i18n.getFixedT(lng, "deploy");

  test(`${lng}: the real AMD refusal shows the required and reported architectures`, () => {
    assert.equal(amd.params?.required_arch_id, "gfx1151");
    const text = placementRefusalText(t, amd);
    assert.equal(text.includes("gfx1151"), true);
    assert.equal(text.includes("gfx942"), true);
    assert.equal(text.includes("MI300X"), true);
    assert.doesNotMatch(text, /CC|undefined|\{\{/);
  });

  test(`${lng}: the existing NVIDIA fixture keeps its compute-capability explanation`, () => {
    const reason = nvidia.eligible_nodes?.flatMap((node) => node.reasons ?? [])
      .find((value) => typeof value !== "string" && value.code === "chip_mismatch");
    assert.equal(reason !== undefined, true);
    const text = placementRefusalText(t, reason);
    assert.equal(text.includes(">=8.9"), true);
    assert.equal(text.includes("A100"), true);
    assert.doesNotMatch(text, /gfx|undefined|\{\{/);
  });
}

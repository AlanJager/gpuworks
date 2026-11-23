import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import { createInstance } from "i18next";

import { APIError, apiErrorText } from "../src/apiError.ts";

// An unavailable recommendation names the recommendation's own reason in
// the user's language; its wording lives with the deploy form's copy.

async function textFor(lng: "en" | "zh", params: Record<string, unknown>) {
  const read = (namespace: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lng}/${namespace}.json`, import.meta.url), "utf8"));
  const i18n = createInstance();
  await i18n.init({ lng, resources: { [lng]: { common: read("common"), deploy: read("deploy") } }, defaultNS: "common", interpolation: { escapeValue: false } });
  return apiErrorText((key, options) => i18n.t(key, options), new APIError("DISTRIBUTED_PARALLELISM_UNAVAILABLE", params, "fallback", 400));
}

test("a recommendation with no layout says so beside the nodes it tried", async () => {
  const params = { node: "gpu-a, gpu-b", reason: "no_layout" };
  assert.equal(
    await textFor("en", params),
    "Cannot recommend parallelism on gpu-a, gpu-b: No TP and PP combination fits the stated degrees and these nodes' GPU counts. Specify TP and PP to use a fixed configuration.",
  );
  assert.equal(
    await textFor("zh", params),
    "无法在 gpu-a, gpu-b 上推荐并行度：没有符合已填并行度和这些节点 GPU 数量的 TP、PP 组合。可填写 TP 和 PP 使用固定配置。",
  );
});

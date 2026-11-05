import assert from "node:assert/strict";
import { test } from "node:test";
import { matchingProfiles } from "../src/pages/deploy/templateRecommendationProfiles.ts";

test("recommendations consume server compatibility instead of interpreting GPU regex", () => {
  const templates = [{ model_id: "qwen/model", scenarios: ["balanced"], profiles: [
    { name: "go-regex", detected_compatible: true, gpus: { vendor: "amd", name: "(?i)radeon", arch_id: "gfx1151" } },
    { name: "unsupported", detected_compatible: false, gpus: { vendor: "amd" } },
  ] }] as never;
  const profiles = matchingProfiles(templates, { source_id: "qwen/model" } as never, "balanced", "amd");
  assert.deepEqual(profiles.map(({ profile, compatible }) => [profile.name, compatible]), [["go-regex", true], ["unsupported", false]]);
});

test("a model repository matches its template without repeating hardware decisions", () => {
  const templates = [{ model_id: "qwen/qwen3-0.6b", scenarios: ["balanced"], profiles: [{
    name: "int8-vllm-ascend", detected_compatible: true, weight_repo_ids: ["vllm-ascend/Qwen3-0.6B-W8A16"], gpus: { vendor: "ascend" },
  }] }] as never;
  const profiles = matchingProfiles(templates, { source_id: "vllm-ascend/Qwen3-0.6B-W8A16" } as never);
  assert.equal(profiles.length, 1);
  assert.equal(profiles[0].profile.name, "int8-vllm-ascend");
});

import assert from "node:assert/strict";
import { test } from "node:test";

import type { InferenceSpec } from "../src/api.ts";
import { matchingProfiles } from "../src/pages/deploy/TemplateRecommendations.tsx";
import { chipMatchesPredicate } from "../src/pages/inferenceTemplateFlat.ts";

// The one chip judgement left in the client: the recommendation list renders
// before any draft exists, so it reads the predicate itself.
const nodeMatchesTemplateSpec = (
  node: {
    gpus?: Array<{
      vendor?: string;
      name?: string;
      compute_capability?: string;
    }>;
  },
  spec?: { gpus?: { vendor: string; cc?: string; name?: string } },
) =>
  Boolean(spec?.gpus) &&
  (node.gpus || []).some((gpu) =>
    chipMatchesPredicate(
      { vendor: gpu.vendor || "", name: gpu.name, cc: gpu.compute_capability },
      spec!.gpus!,
    ),
  );
const specMatchesAnyNode = (
  nodes: Array<{
    gpus?: Array<{
      vendor?: string;
      name?: string;
      compute_capability?: string;
    }>;
  }>,
  spec?: { gpus?: { vendor: string; cc?: string; name?: string } },
) =>
  Boolean(spec?.gpus) &&
  nodes.some((node) => nodeMatchesTemplateSpec(node, spec));
const anyNodeMatchesGPUPredicate = specMatchesAnyNode;
// chipsOf is the card models a set of nodes has, as GET /nodes/facets reports
// them: what the recommendation list now judges.
const chipsOf = (nodes: Array<{ gpus?: Array<{ vendor?: string; name?: string; compute_capability?: string }> }>) =>
  nodes.flatMap((node) => (node.gpus || []).map((gpu) => ({ vendor: gpu.vendor || "", name: gpu.name || "", cc: gpu.compute_capability })));

const turing = {
  uuid: "turing",
  gpus: [
    { index: 0, vendor: "nvidia", name: "Tesla T4", compute_capability: "7.5" },
  ],
} as never;
const ascend = {
  uuid: "ascend",
  gpus: [{ index: 0, vendor: "ascend", name: "910B4", compute_capability: "" }],
} as never;

// The four specs of the reported ASR model: every one is "balanced", and the
// Ascend spec sorts first.
const asrSelections = [
  {
    spec: {
      name: "bf16-sglang-ascend",
      scenario: "balanced",
      engine: "sglang",
      source: "builtin",
      gpus: { vendor: "ascend" },
    },
  },
  {
    spec: {
      name: "bf16-vllm-nvidia-turing",
      scenario: "balanced",
      engine: "vllm",
      source: "builtin",
      gpus: { vendor: "nvidia", cc: "7.5" },
    },
  },
  {
    spec: {
      name: "fp8-vllm-nvidia-general",
      scenario: "balanced",
      engine: "vllm",
      source: "builtin",
      gpus: { vendor: "nvidia" },
    },
  },
  {
    spec: {
      name: "w8a8-sglang-ascend",
      scenario: "balanced",
      engine: "sglang",
      source: "builtin",
      gpus: { vendor: "ascend" },
    },
  },
] as never as Array<{ spec: InferenceSpec }>;

test("the scenario buttons and the recommendation list judge compatibility identically", () => {
  const nodes = [turing, ascend];
  for (const { spec } of asrSelections) {
    const profiles = matchingProfiles(
      [
        {
          model_id: "qwen/qwen3-asr",
          scenarios: ["balanced"],
          profiles: [{ gpus: spec.gpus }],
        },
      ] as never,
      chipsOf(nodes),
      { source_id: "Qwen/Qwen3-ASR", name: "asr" } as never,
    );
    assert.equal(
      profiles[0].compatible,
      specMatchesAnyNode(nodes, spec),
      `compatibility disagreed for ${spec.name}`,
    );
  }
});

test("a model repository used by a spec matches its template without revision matching", () => {
  const profiles = matchingProfiles(
    [
      {
        model_id: "qwen/qwen3-0.6b",
        scenarios: ["balanced"],
        profiles: [
          {
            name: "int8-vllm-ascend",
            weight_repo_ids: ["vllm-ascend/Qwen3-0.6B-W8A16"],
            gpus: { vendor: "ascend" },
          },
        ],
      },
    ] as never,
    chipsOf([ascend]),
    {
      source_id: "vllm-ascend/Qwen3-0.6B-W8A16",
      name: "vllm-ascend/Qwen3-0.6B-W8A16",
    } as never,
  );
  assert.equal(profiles.length, 1);
  assert.equal(profiles[0].profile.name, "int8-vllm-ascend");
});

test("compatibility helpers handle nodes and predicates that carry nothing", () => {
  assert.equal(nodeMatchesTemplateSpec(turing, undefined), false);
  assert.equal(
    specMatchesAnyNode([], { name: "x", gpus: { vendor: "nvidia" } } as never),
    false,
  );
  assert.equal(anyNodeMatchesGPUPredicate([turing], undefined), false);
  assert.equal(
    anyNodeMatchesGPUPredicate([{ uuid: "bare" } as never], {
      vendor: "nvidia",
    }),
    false,
    "a node with no reported GPUs matches nothing",
  );
});

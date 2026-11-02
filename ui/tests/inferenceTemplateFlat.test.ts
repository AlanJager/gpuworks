import assert from "node:assert/strict";
import { test } from "node:test";

import { chipDisplayLabel, chipKey, chipMatchesPredicate, compactModelMetadata, modelMatchesChip, predicateLabel, preferredTemplateEngine, templateModelOperations, specDisplayName } from "../src/pages/inferenceTemplateFlat.ts";

test("detected GPU labels prefer a concise model and show CC only for generic devices", () => {
  assert.equal(chipDisplayLabel({ vendor: "nvidia", name: "NVIDIA GeForce RTX 2080 Ti", cc: "7.5" }), "RTX 2080 Ti");
  assert.equal(chipDisplayLabel({ vendor: "nvidia", name: "Quadro RTX 8000", cc: "7.5" }), "RTX 8000");
  assert.equal(chipDisplayLabel({ vendor: "nvidia", name: "NVIDIA A40", cc: "8.6" }), "A40");
  assert.equal(chipDisplayLabel({ vendor: "ascend", name: "Ascend 910B4" }), "910B4");
  assert.equal(chipDisplayLabel({ vendor: "nvidia", name: "NVIDIA vGPU-32GB", cc: "8.9" }), "vGPU 32GB · CC 8.9");
});

test("flat GPU predicates support exact, range, open range, and name regex", () => {
  const h100 = { vendor: "NVIDIA", name: "NVIDIA H100 80GB HBM3", cc: "9.0" };
  assert.equal(chipMatchesPredicate(h100, { vendor: "nvidia", cc: "9.0", name: "\\bH100\\b" }), true);
  assert.equal(chipMatchesPredicate(h100, { vendor: "nvidia", cc: "8.9..9.0" }), true);
  assert.equal(chipMatchesPredicate(h100, { vendor: "nvidia", cc: ">=10.0" }), false);
  assert.equal(chipMatchesPredicate(h100, { vendor: "nvidia", cc: "<9.0" }), false);
  assert.equal(chipMatchesPredicate(h100, { vendor: "ascend" }), false);
  assert.equal(chipMatchesPredicate(h100, { vendor: "nvidia", name: "[" }), false);
});

test("selected chip filters models using their inline spec predicates", () => {
  const model = { specs: [{ gpus: { vendor: "ascend", name: "910B4" } }] } as never;
  assert.equal(modelMatchesChip(model, { vendor: "ascend", name: "Ascend 910B4" }), true);
  assert.equal(modelMatchesChip(model, { vendor: "nvidia", name: "NVIDIA H100", cc: "9.0" }), false);
  assert.equal(chipKey({ vendor: "NVIDIA", name: "H100", cc: "9.0" }), "nvidia\u0000H100\u00009.0");

  const hygon = { specs: [{ gpus: { vendor: "hygon", name: "(^|[^A-Za-z0-9])K100[_ -]?AI([^A-Za-z0-9]|$)" } }] } as never;
  assert.equal(modelMatchesChip(hygon, { vendor: "hygon", name: "K100_AI" }), true);
  assert.equal(modelMatchesChip(hygon, { vendor: "hygon", name: "BW1000" }), false);

  const vetoed = { specs: [{ quantization: "fp8", gpus: { vendor: "nvidia", cc: ">=8.9" } }], unsupported: [{ quantization: "fp8", gpus: { vendor: "nvidia", name: "H100" }, reason: "known failure" }] } as never;
  assert.equal(modelMatchesChip(vetoed, { vendor: "nvidia", name: "NVIDIA H100", cc: "9.0" }), false);
});

test("model detail prefers vLLM before hardware deployability", () => {
  const specs = [
    { engine: "vllm", gpus: { vendor: "nvidia", cc: "9.0" } },
    { engine: "sglang", gpus: { vendor: "ascend", name: "910B4" } },
  ] as never;
  assert.equal(preferredTemplateEngine(specs, [{ vendor: "ascend", name: "Ascend 910B4" }]), "vllm");
  assert.equal(preferredTemplateEngine(specs, []), "vllm");
});

test("model operations and metadata stay concise for catalog presentation", () => {
  const model = {
    model_id: "deepseek-ai/deepseek-v4-flash-0731",
    parameter_count: "291.04B advertised checkpoint scale; quantized tensor payload is 314,621,886,200 bytes",
    architecture: "DeepseekV4ForCausalLM; 43 layers, hidden size 4096, 256 routed experts plus one shared expert",
    context_length: 1048576,
    input_modalities: ["text"],
    specs: [{ api: { operations: [{ type: "openai.chat" }] } }],
  } as never;
  assert.equal(compactModelMetadata(model), "291.04B · moe · 1,048,576 ctx");
  assert.deepEqual(templateModelOperations(model), ["openai.chat"]);
  assert.deepEqual(templateModelOperations({ model_id: "org/embedding", specs: [{ args: ["--runner", "pooling"] }] } as never), [], "the UI must not infer operation from names or argv");
});

test("detail presentation names the GPU predicate with its compute capability and name pattern", () => {
  const spec = { gpus: { vendor: "nvidia", cc: "9.0", name: "\\bH100\\b" } };
  assert.equal(predicateLabel(spec as never), "NVIDIA · CC 9.0 · \\bH100\\b");
});

test("spec display names are the catalog's own per-locale names, never derived", () => {
  const spec = { name: "opaque-id", display_name_zh_cn: "H20 / H200 · FP8 · 8 卡", display_name_en: "H20 / H200 · FP8 · 8 GPUs", gpus: { vendor: "nvidia", name: "unreadable-regex" } };
  assert.equal(specDisplayName(spec, "zh"), "H20 / H200 · FP8 · 8 卡");
  assert.equal(specDisplayName(spec, "zh-CN"), "H20 / H200 · FP8 · 8 卡");
  assert.equal(specDisplayName(spec, "en"), "H20 / H200 · FP8 · 8 GPUs");
  assert.equal(specDisplayName({ ...spec, display_name_en: "" }, "en"), "H20 / H200 · FP8 · 8 卡", "falls back to the other language");
  assert.equal(specDisplayName({ name: "user-spec" }, "zh"), "user-spec", "an unnamed user spec shows its id");
});

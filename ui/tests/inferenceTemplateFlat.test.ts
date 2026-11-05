import assert from "node:assert/strict";
import { test } from "node:test";

import { chipDisplayLabel, chipKey, compactModelMetadata, predicateLabel, templateModelOperations, specDisplayName } from "../src/pages/inferenceTemplateFlat.ts";

test("detected GPU labels prefer a concise model and show CC only for generic devices", () => {
  assert.equal(chipDisplayLabel({ vendor: "nvidia", name: "NVIDIA GeForce RTX 2080 Ti", cc: "7.5" }), "RTX 2080 Ti");
  assert.equal(chipDisplayLabel({ vendor: "nvidia", name: "Quadro RTX 8000", cc: "7.5" }), "RTX 8000");
  assert.equal(chipDisplayLabel({ vendor: "nvidia", name: "NVIDIA A40", cc: "8.6" }), "A40");
  assert.equal(chipDisplayLabel({ vendor: "ascend", name: "Ascend 910B4" }), "910B4");
  assert.equal(chipDisplayLabel({ vendor: "amd", name: "Radeon 8060S", arch_id: "gfx1151" }), "Radeon 8060S · gfx1151");
  assert.equal(chipDisplayLabel({ vendor: "nvidia", name: "NVIDIA vGPU-32GB", cc: "8.9" }), "vGPU 32GB · CC 8.9");
});







test("chip identity retains reported architecture without deciding compatibility", () => {
  assert.equal(chipKey({ vendor: "NVIDIA", name: "H100", cc: "9.0" }), "nvidia\u0000H100\u00009.0\u0000");
  assert.notEqual(chipKey({ vendor: "amd", name: "GPU", arch_id: "gfx1150" }), chipKey({ vendor: "amd", name: "GPU", arch_id: "gfx1151" }));
  assert.equal(chipKey({ vendor: "amd", name: "GPU", arch_id: "GFX1151" }), chipKey({ vendor: "amd", name: "GPU", arch_id: "gfx1151" }));
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
  assert.equal(predicateLabel({ gpus: { vendor: "amd", arch_id: "gfx1151" } } as never), "AMD · arch gfx1151");
});

test("spec display names are the catalog's own per-locale names, never derived", () => {
  const spec = { name: "opaque-id", display_name_zh_cn: "H20 / H200 · FP8 · 8 卡", display_name_en: "H20 / H200 · FP8 · 8 GPUs", gpus: { vendor: "nvidia", name: "unreadable-regex" } };
  assert.equal(specDisplayName(spec, "zh"), "H20 / H200 · FP8 · 8 卡");
  assert.equal(specDisplayName(spec, "zh-CN"), "H20 / H200 · FP8 · 8 卡");
  assert.equal(specDisplayName(spec, "en"), "H20 / H200 · FP8 · 8 GPUs");
  assert.equal(specDisplayName({ ...spec, display_name_en: "" }, "en"), "H20 / H200 · FP8 · 8 卡", "falls back to the other language");
  assert.equal(specDisplayName({ name: "user-spec" }, "zh"), "user-spec", "an unnamed user spec shows its id");
});

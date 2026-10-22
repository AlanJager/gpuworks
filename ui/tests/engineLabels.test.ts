import assert from "node:assert/strict";
import test from "node:test";
import { engineDisplayName, engineOptionLabel } from "../src/engineLabels";

test("engine labels preserve the canonical identity", () => {
  assert.equal(engineDisplayName("vllm"), "vLLM");
  assert.equal(engineDisplayName("sglang"), "SGLang");
  assert.equal(engineDisplayName("callable"), "Callable");
  assert.equal(engineDisplayName("custom-engine"), "custom-engine");
  assert.equal(engineOptionLabel("sglang"), "SGLang · sglang");
  assert.equal(engineOptionLabel("callable"), "Callable · callable");
  assert.equal(engineOptionLabel("custom-engine"), "custom-engine");
});

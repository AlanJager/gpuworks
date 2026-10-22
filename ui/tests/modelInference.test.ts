import assert from "node:assert/strict";
import { test } from "node:test";
import { hasOperation, inputModalitiesOf, operationTypesOf } from "../src/modelInference.ts";

test("model inference presentation derives only from declared operations and input types", () => {
  const operations = operationTypesOf([{ type: "openai.chat" }, { type: "openai.chat" }, { type: "openai.embeddings" }] as never);
  assert.deepEqual(operations, ["openai.chat", "openai.embeddings"]);
  assert.equal(hasOperation(operations.map((type) => ({ type })) as never, "openai.chat"), true);
  assert.equal(hasOperation(operations.map((type) => ({ type })) as never, "openai.image.generate"), false);
  assert.deepEqual(inputModalitiesOf([" IMAGE ", "text", "image", "unknown"]), ["image", "text"]);
});

test("empty operations and input types stay empty instead of falling back to chat or text", () => {
  assert.deepEqual(operationTypesOf([]), []);
  assert.deepEqual(inputModalitiesOf([]), []);
  assert.equal(hasOperation(undefined, "openai.chat"), false);
});

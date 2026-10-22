import assert from "node:assert/strict";
import { test } from "node:test";

import type { Model } from "../src/api.ts";
import { refreshRemoteModelMetadata } from "../src/pages/models/modelMetadataRefresh.ts";

function model(source: string): Model {
  return {
    id: 7,
    source,
    source_id: "Qwen/Qwen3-8B",
    revision: "",
    name: "Qwen3-8B",
    arch: "",
    quant_label: "",
    weight_gib: 0,
    capability: "chat",
    created_at: "2026-08-31T00:00:00Z",
  };
}

test("Hugging Face refresh fetches metadata in the browser before persisting it", async () => {
  const current = model("HuggingFace");
  const calls: unknown[][] = [];
  const updated = { ...current, weight_gib: 1 };
  const result = await refreshRemoteModelMetadata(
    current,
    async (id, metadata) => {
      calls.push(["persist", id, metadata]);
      return updated;
    },
    async (repo, revision) => {
      calls.push(["fetch", repo, revision]);
      return { readme: "# browser", config_raw: "{}", weight_bytes: 1234, logo_data: "" };
    },
  );

  assert.equal(result, updated);
  assert.deepEqual(calls, [
    ["fetch", "Qwen/Qwen3-8B", "main"],
    ["persist", 7, {
      readme: "# browser",
      config_raw: "{}",
      weight_bytes: 1234,
      logo_data: "",
      browser_metadata: true,
    }],
  ]);
});

test("ModelScope refresh keeps the server-side metadata path", async () => {
  const current = model("modelscope");
  let browserFetches = 0;
  let persistedMetadata: unknown = "not-called";
  await refreshRemoteModelMetadata(
    current,
    async (_id, metadata) => {
      persistedMetadata = metadata;
      return current;
    },
    async () => {
      browserFetches++;
      throw new Error("unexpected browser fetch");
    },
  );

  assert.equal(browserFetches, 0);
  assert.equal(persistedMetadata, undefined);
});

test("a Hugging Face refresh the browser cannot read goes to the server without the browser marker", async () => {
  const current = model("huggingface");
  const calls: unknown[][] = [];
  const result = await refreshRemoteModelMetadata(
    current,
    async (id, metadata) => { calls.push(["persist", id, metadata]); return current; },
    async () => { throw new Error("no address is allowed to the browser"); },
  );
  assert.equal(result, current);
  assert.deepEqual(calls, [["persist", 7, undefined]], "the server reads; nothing claims the browser did");
});

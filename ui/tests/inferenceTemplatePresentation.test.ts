import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import {
  modelOrganizationIcon,
  modelOrganizationIconOrganizations,
  modelOrganizationGroups,
  modelOrganizationKey,
  modelOrganizationName,
} from "../src/pages/inferenceTemplatePresentation.ts";

const messages: Record<string, string> = {
  "organization.other": "其他",
};
const t = ((key: string, options: Record<string, unknown> = {}) => Object.entries(options).reduce(
  (value, [name, replacement]) => value.replaceAll(`{{${name}}}`, String(replacement)), messages[key] || key,
)) as never;

test("model organizations use original model IDs rather than variant repositories", () => {
  assert.equal(modelOrganizationKey("baidu/ernie-4.5"), "baidu");
  assert.equal(modelOrganizationKey("qwen3-14b-q4_k_m.gguf"), "other");
  assert.equal(modelOrganizationName("baidu", t), "Baidu");
  assert.equal(modelOrganizationName("bytedance-seed", t), "ByteDance");
  assert.deepEqual(modelOrganizationGroups([
    { model_id: "bytedance-seed/seed-oss" },
    { model_id: "baidu/ernie-4.5" },
    { model_id: "baidu/ernie-4.5" },
    { model_id: "baidu/unlimited-ocr" },
  ], t), [
    { key: "baidu", label: "Baidu", modelCount: 2 },
    { key: "bytedance-seed", label: "ByteDance", modelCount: 1 },
  ]);
});

test("model organization icons use only reviewed local assets", () => {
  assert.equal(modelOrganizationIconOrganizations.length, 60);
  assert.equal(modelOrganizationIcon("deepseek-ai/deepseek-r1"), "/model-org-icons/deepseek-ai.webp");
  assert.equal(modelOrganizationIcon("eco-tech/deepseek-v3.1-w8a8"), undefined);
  assert.equal(modelOrganizationIcon("zhipuai/glm-5"), undefined);
  assert.equal(modelOrganizationIcon("qwen3-14b-q4_k_m.gguf"), undefined);
});

test("every reviewed organization icon is a 96 by 96 WebP", async () => {
  for (const organization of modelOrganizationIconOrganizations) {
    const data = await readFile(new URL(`../public/model-org-icons/${organization}.webp`, import.meta.url));
    assert.equal(data.subarray(0, 4).toString("ascii"), "RIFF", organization);
    assert.equal(data.subarray(8, 12).toString("ascii"), "WEBP", organization);
    const chunk = data.subarray(12, 16).toString("ascii");
    let width = 0;
    let height = 0;
    if (chunk === "VP8 ") {
      width = data.readUInt16LE(26) & 0x3fff;
      height = data.readUInt16LE(28) & 0x3fff;
    } else if (chunk === "VP8L") {
      width = 1 + ((data[21] | (data[22] << 8)) & 0x3fff);
      height = 1 + (((data[22] >> 6) | (data[23] << 2) | (data[24] << 10)) & 0x3fff);
    } else if (chunk === "VP8X") {
      width = 1 + data.readUIntLE(24, 3);
      height = 1 + data.readUIntLE(27, 3);
    }
    assert.deepEqual([width, height], [96, 96], organization);
  }
});

test("Hygon compute vendor uses the reviewed horizontal WebP logo", async () => {
  const data = await readFile(new URL("../public/vendor-logos/hygon.webp", import.meta.url));
  assert.equal(data.subarray(0, 4).toString("ascii"), "RIFF");
  assert.equal(data.subarray(8, 12).toString("ascii"), "WEBP");
});

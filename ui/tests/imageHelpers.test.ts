import assert from "node:assert/strict";
import { test } from "node:test";

import { digestMismatchNodes, shortDigest } from "../src/pages/images/ImagePageHelpers.tsx";

test("a short digest is 12 characters without the sha256: prefix", () => {
  assert.equal(shortDigest("sha256:0123456789abcdef0123"), "0123456789ab");
  assert.equal(shortDigest("0123456789abcdef"), "0123456789ab");
  assert.equal(shortDigest(undefined), "");
});

const node = (uuid: string, extra: Record<string, unknown> = {}) => ({ node_uuid: uuid, node_name: uuid, node_state: "online", state: "present", updated_at: "2026-10-08T00:00:00Z", ...extra });
const version = (nodes: unknown[], extra: Record<string, unknown> = {}) => ({ id: 1, image_ref: "vllm/vllm-openai:nightly", nodes, ...extra }) as never;

test("digests disagree only when two or more nodes recorded different ones", () => {
  assert.equal(digestMismatchNodes(version([node("a", { image_digest_id: "sha256:1" }), node("b", { image_digest_id: "sha256:2" }), node("c")])), 2);
  assert.equal(digestMismatchNodes(version([node("a", { image_digest_id: "sha256:1" }), node("b", { image_digest_id: "sha256:1" })])), 0);
  assert.equal(digestMismatchNodes(version([node("a", { image_digest_id: "sha256:1" }), node("b")])), 0, "a node without a digest is not a disagreement");
  assert.equal(digestMismatchNodes(version([])), 0);
});

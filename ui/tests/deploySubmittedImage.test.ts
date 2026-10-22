import assert from "node:assert/strict";
import { test } from "node:test";

// The deploy form's image contract: everything it displayed is confirmed by
// the user, so the submitted request must carry exactly that ref. This is
// what keeps the server from re-resolving a default the user never saw
// change — the 2026-09 incident where auto mode deployed something other
// than the form preview showed.
const { submittedImage } = await import("../src/pages/deploy/deployHelpers.ts");

test("an explicit pick is submitted unchanged", () => {
  assert.equal(submittedImage("registry/team/image:v2", "gpustack/runner:cuda13.0-vllm0.27.1"), "registry/team/image:v2");
});

test("auto mode submits the evaluated ref the form displayed", () => {
  assert.equal(submittedImage(undefined, "gpustack/runner:cuda13.0-vllm0.27.1"), "gpustack/runner:cuda13.0-vllm0.27.1");
  assert.equal(submittedImage("", "gpustack/runner:cuda13.0-vllm0.27.1"), "gpustack/runner:cuda13.0-vllm0.27.1");
});

test("nothing shown claims nothing, leaving resolution to the server", () => {
  assert.equal(submittedImage(undefined, undefined), undefined);
  assert.equal(submittedImage("", ""), undefined);
  assert.equal(submittedImage("  ", null), undefined);
});

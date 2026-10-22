import assert from "node:assert/strict";
import { test } from "node:test";
import { hostOf, kibToMiB, miBToKiB, moved, picksToRequirements, requirementChips, requirementPicks } from "../src/pages/settings/pull/pullFormat.ts";

test("hostOf reduces an endpoint or a credential to its registry host", () => {
  assert.equal(hostOf("registry.zstack.ai/gpuworks/"), "registry.zstack.ai");
  assert.equal(hostOf("https://HF-Mirror.com"), "hf-mirror.com");
  assert.equal(hostOf("harbor.corp.example:5000/dockerhub-proxy"), "harbor.corp.example:5000");
  assert.equal(hostOf("user:pw@registry.example/x"), "registry.example");
  assert.equal(hostOf("  "), "");
});

test("a node selector round-trips through the picks the control shows", () => {
  const requirements = { site: ["idc-a", "idc-b"], "gpuworks.ai/provider": ["runpod"] };
  assert.deepEqual(requirementPicks(requirements), ["site=idc-a", "site=idc-b", "gpuworks.ai/provider=runpod"]);
  assert.deepEqual(picksToRequirements(requirementPicks(requirements)), requirements);
  // Values of one key are alternatives; junk and duplicates are dropped.
  assert.deepEqual(picksToRequirements(["site=a", "site=a", "nokey", "=x", "k="]), { site: ["a"] });
  assert.deepEqual(picksToRequirements(["note=a=b"]), { note: ["a=b"] });
  assert.deepEqual(requirementChips({ site: ["a", "b"], empty: [] }), ["site=a | b"]);
  assert.deepEqual(requirementChips({}), []);
});

test("rates convert between the form's MB/s and the API's KiB/s", () => {
  assert.equal(miBToKiB(200), 204800);
  assert.equal(miBToKiB(0.5), 512);
  assert.equal(miBToKiB(0.0001), 1, "a positive limit is never zero");
  assert.equal(miBToKiB(0), 0);
  assert.equal(kibToMiB(204800), 200);
  assert.equal(kibToMiB(500), 0.49);
});

test("moved shifts one item and leaves the list alone at the ends", () => {
  const list = ["a", "b", "c"];
  assert.deepEqual(moved(list, 2, -1), ["a", "c", "b"]);
  assert.equal(moved(list, 0, -1), list);
  assert.equal(moved(list, 2, 1), list);
  assert.deepEqual(list, ["a", "b", "c"], "the input is not mutated");
});

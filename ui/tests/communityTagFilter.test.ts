import assert from "node:assert/strict";
import { test } from "node:test";

import { searchCommunityTags } from "../src/pages/images/tagFilter.ts";

const tags = [
  { tag: "v0.11.0" },
  { tag: "qwen38", engine_version: "" },
  { tag: "nightly", rolling_tag: true },
  { tag: "nightly-20261007", rolling_tag: false },
  { tag: "latest", rolling_tag: true },
  { tag: "release-abcdef1" },
];

test("an empty search keeps every upstream tag, pre-release and hash tags included", () => {
  assert.deepEqual(searchCommunityTags(tags, "").map((item) => item.tag), tags.map((item) => item.tag));
  assert.deepEqual(searchCommunityTags(tags, "   ").length, tags.length);
});

test("searching matches tag names case-insensitively by substring", () => {
  assert.deepEqual(searchCommunityTags(tags, "qwen38").map((item) => item.tag), ["qwen38"]);
  assert.deepEqual(searchCommunityTags(tags, "NIGHT").map((item) => item.tag), ["nightly", "nightly-20261007"]);
  assert.deepEqual(searchCommunityTags(tags, "no-such-tag"), []);
});

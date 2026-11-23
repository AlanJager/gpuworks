// Where a model transfer's bytes come from, as the model files table and a
// deployment's model preparation both show it.
import assert from "node:assert/strict";
import { test } from "node:test";
import type { TFunction } from "i18next";
import { remainingText } from "../src/format.ts";
import { transferEtaSeconds, transferOrigin } from "../src/pages/models/modelPresenceDisplay.ts";
import { createI18n } from "./helpers/dom.ts";

test("a copy names its source node, a download the host the agent reports", () => {
  assert.deepEqual(transferOrigin({ acquisition: "peer", source_node_name: "gpu3", source_node_uuid: "u1" }), { node: "gpu3" });
  assert.deepEqual(transferOrigin({ acquisition: "peer", source_node_uuid: "0123456789abcdef" }), { node: "01234567" }, "an unnamed node by its short uuid");
  assert.deepEqual(transferOrigin({ acquisition: "download", message: "downloading from hf-mirror.com" }), { host: "hf-mirror.com" });
  assert.deepEqual(transferOrigin({ acquisition: "download", message: " downloading from modelscope.cn:443 " }), { host: "modelscope.cn:443" });
  assert.deepEqual(
    transferOrigin({ acquisition: "download", message: "downloading from huggingface.co (switched: hf-mirror.com stalled 2 times in a row: https://hf-mirror.com/x)" }),
    { host: "huggingface.co", switched: "hf-mirror.com stalled 2 times in a row: https://hf-mirror.com/x" },
    "after a source failed, the host it moved to and why",
  );
});

test("anything else says nothing about a source", () => {
  assert.equal(transferOrigin({ acquisition: "download", message: "list files: 503" }), null, "a failure is not a source");
  assert.equal(transferOrigin({ acquisition: "download" }), null);
  assert.equal(transferOrigin({ acquisition: "peer" }), null, "a copy whose source is not known yet");
  assert.equal(transferOrigin({}), null);
});

const GiB = 1024 ** 3;
const MiB = 1024 ** 2;

test("a download's remaining time follows from its bytes and rate", () => {
  const moving = { state: "downloading", acquisition: "download", size_bytes: 181 * GiB, downloaded: 98.8 * GiB, rate_bps: 14 * MiB };
  assert.equal(transferEtaSeconds(moving), 6013);
  assert.equal(transferEtaSeconds({ ...moving, state: "syncing", acquisition: "peer" }), 6013, "a node-to-node sync is estimated the same way");
  assert.equal(transferEtaSeconds({ ...moving, rate_bps: 0 }), 0, "no rate, no estimate");
  assert.equal(transferEtaSeconds({ ...moving, size_bytes: 0 }), 0, "no total, no estimate");
  assert.equal(transferEtaSeconds({ ...moving, downloaded: 181 * GiB }), 0, "nothing left");
  assert.equal(transferEtaSeconds({ ...moving, state: "failed" }), 0, "a stopped transfer keeps its last rate on the row");
  assert.equal(transferEtaSeconds({ ...moving, preparation: { phase: "downloading", total_known: false, idle_seconds: 0 } }), 0, "a total the server does not vouch for");
});

test("a cache copy's remaining time is the server's alone", () => {
  const copy = { state: "downloading", acquisition: "copy", size_bytes: 100, downloaded: 40, rate_bps: 10 };
  assert.equal(transferEtaSeconds({ ...copy, preparation: { phase: "copying", total_known: true, idle_seconds: 0, eta_seconds: 9 } }), 9);
  assert.equal(transferEtaSeconds({ ...copy, preparation: { phase: "copying", total_known: true, idle_seconds: 0 } }), 0, "withheld until the rate is stable");
});

test("remaining time reads in the unit an operator plans with", async () => {
  for (const [lng, want] of [
    ["en", ["under 1 min left", "about 1 min left", "about 3 min left", "about 1 h left", "about 1 h 41 min left", "about 26 h left"]],
    ["zh", ["剩余不到 1 分钟", "剩余约 1 分钟", "剩余约 3 分钟", "剩余约 1 小时", "剩余约 1 小时 41 分钟", "剩余约 26 小时"]],
  ] as const) {
    const { i18n } = await createI18n([], lng);
    const t = i18n.t.bind(i18n) as TFunction;
    assert.deepEqual([59, 60, 160, 3541, 6013, 26 * 3600].map((seconds) => remainingText(t, seconds)), want);
  }
});

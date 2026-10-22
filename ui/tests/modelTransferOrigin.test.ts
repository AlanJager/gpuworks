// Where a model transfer's bytes come from, as the model files table and a
// deployment's model preparation both show it.
import assert from "node:assert/strict";
import { test } from "node:test";
import { transferOrigin } from "../src/pages/models/modelPresenceDisplay.ts";

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

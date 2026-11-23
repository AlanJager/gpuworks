import assert from "node:assert/strict";
import { test } from "node:test";
import { cardOK, isCardKind } from "../src/pages/networks/cardTest.ts";

test("card kinds and a card pair's outcome", () => {
  assert.equal(isCardKind("card_ping") && isCardKind("card_matrix") && !isCardKind("tcp") && !isCardKind("rdma_tool") && !isCardKind(undefined), true);
  assert.equal(cardOK({ rail: "npu3", target_rail: "npu3", gbps: 0, reachable: true, passed: true, rtt_ms: 0.25 }), true);
  assert.equal(cardOK({ rail: "npu3", target_rail: "npu4", gbps: 0, passed: false, code: "unreachable", error: "no answer within the time limit" }), false);
  // The server classifies old stored results before serving them.
  assert.equal(cardOK({ rail: "npu3", gbps: 0, passed: false }), false);
});

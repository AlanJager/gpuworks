import assert from "node:assert/strict";
import { test } from "node:test";
import { cardOK, isCardKind } from "../src/pages/networks/cardTest.ts";

test("card kinds and a card pair's outcome", () => {
  assert.equal(isCardKind("card_ping") && isCardKind("card_matrix") && !isCardKind("tcp") && !isCardKind("rdma_tool") && !isCardKind(undefined), true);
  assert.equal(cardOK({ rail: "npu3", target_rail: "npu3", gbps: 0, reachable: true, rtt_ms: 0.25 }), true);
  assert.equal(cardOK({ rail: "npu3", target_rail: "npu4", gbps: 0, code: "unreachable", error: "no answer within the time limit" }), false);
  // Answered is the only success: a row with no outcome at all is not one.
  assert.equal(cardOK({ rail: "npu3", gbps: 0 }), false);
});

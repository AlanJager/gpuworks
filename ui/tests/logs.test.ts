import assert from "node:assert/strict";
import test from "node:test";

import { ansiToSegments, decodeLogEvent } from "../src/pages/logStream.ts";

test("decodeLogEvent unwraps legacy JSON SSE strings and unicode escapes", () => {
  assert.equal(decodeLogEvent('"\\u001b[32mINFO\\u001b[0m <ok>"'), "\u001b[32mINFO\u001b[0m <ok>");
});

test("decodeLogEvent leaves plain-text SSE payloads unchanged", () => {
  assert.equal(decodeLogEvent("INFO ready"), "INFO ready");
  assert.equal(decodeLogEvent('not a complete "json string'), 'not a complete "json string');
});

test("ansiToSegments maps common colors and preserves tabs", () => {
  assert.deepEqual(ansiToSegments("before\x1b[36mcyan\x1b[0m\tafter"), [
    { text: "before", style: {} },
    { text: "cyan", style: { color: "#11a8cd" } },
    { text: "\tafter", style: {} },
  ]);
});

test("ansiToSegments filters non-text terminal controls", () => {
  assert.deepEqual(ansiToSegments("ok\x1b[2K\x1b]0;title\x07 done\x00"), [
    { text: "ok", style: {} },
    { text: " done", style: {} },
  ]);
});

import assert from "node:assert/strict";
import { test } from "node:test";
import { readSSE } from "../src/sse.ts";

function body(...chunks: string[]): ReadableStream<Uint8Array> {
  const enc = new TextEncoder();
  return new ReadableStream({ start(c) { for (const ch of chunks) c.enqueue(enc.encode(ch)); c.close(); } });
}

test("readSSE yields named events across chunk boundaries and skips comments", async () => {
  const got: [string, string][] = [];
  await readSSE(body(": ping\n\nevent: te", "xt\ndata: {\"delta\":\"a\"}\n", "\nevent: done\r\ndata: {}\r\n\r\ndata: line1\ndata: line2"), (e, d) => got.push([e, d]));
  assert.deepEqual(got, [["text", '{"delta":"a"}'], ["done", "{}"], ["message", "line1\nline2"]]);
});

test("readSSE ignores frames without data", async () => {
  const got: string[] = [];
  await readSSE(body("event: step\n\n: only a comment\n\n"), (e) => got.push(e));
  assert.deepEqual(got, []);
});

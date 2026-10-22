import assert from "node:assert/strict";
import test from "node:test";
import { formatDate, formatTime, formatTimeSec } from "../src/i18n/datetime.ts";

test("date-time helpers use the fixed product format", () => {
  const local = new Date(2026, 7, 25, 4, 3, 2);
  assert.equal(formatTime(local), "2026-08-25 04:03");
  assert.equal(formatTimeSec(local), "2026-08-25 04:03:02");
});

test("date-only metadata is not shifted through Date", () => {
  assert.equal(formatDate("2026-08-25"), "2026-08-25");
  assert.equal(formatTime("not-a-date"), "—");
});

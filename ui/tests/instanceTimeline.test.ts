import assert from "node:assert/strict";
import { test } from "node:test";

import type { InstanceEvent } from "../src/api";
import { mergeLiveSystemLines, mergeSystemLines } from "../src/pages/instanceTimeline";

function event(overrides: Partial<InstanceEvent> & { id: number; created_at: string }): InstanceEvent {
  return { kind: "state", code: "INSTANCE_STATE_CHANGED", params: {}, ...overrides };
}

test("mergeSystemLines interleaves log lines and events in chronological order", () => {
  const logs = [
    { timeMS: 1000, text: "log a" },
    { timeMS: 3000, text: "log b" },
  ];
  const events = [
    event({ id: 1, created_at: new Date(2000).toISOString(), code: "INSTANCE_STATE_CHANGED", params: { state: "running" } }),
  ];
  const merged = mergeSystemLines(logs, events);
  assert.deepEqual(merged.map((m) => m.kind), ["log", "system", "log"]);
  assert.equal(merged[0].kind === "log" ? merged[0].text : undefined, "log a");
  assert.equal(merged[2].kind === "log" ? merged[2].text : undefined, "log b");
});

test("mergeSystemLines is a stable sort: equal timestamps keep log lines ahead of system lines", () => {
  const sameTime = 5000;
  const logs = [{ timeMS: sameTime, text: "log at tie" }];
  const events = [
    event({ id: 1, created_at: new Date(sameTime).toISOString(), params: { state: "running" } }),
  ];
  const merged = mergeSystemLines(logs, events);
  assert.deepEqual(merged.map((m) => m.kind), ["log", "system"]);
});

test("mergeSystemLines keeps relative order among several equal-timestamp log lines", () => {
  const logs = [
    { timeMS: 100, text: "first" },
    { timeMS: 100, text: "second" },
    { timeMS: 100, text: "third" },
  ];
  const merged = mergeSystemLines(logs, []);
  assert.deepEqual(merged.map((m) => (m.kind === "log" ? m.text : "")), ["first", "second", "third"]);
});

test("an unparsable event timestamp sorts to the start instead of throwing", () => {
  const logs = [{ timeMS: 100, text: "log" }];
  const events = [event({ id: 1, created_at: "not-a-date", params: { state: "running" } })];
  const merged = mergeSystemLines(logs, events);
  assert.equal(merged[0].kind, "system");
  assert.equal(merged[1].kind, "log");
});

// --- mergeLiveSystemLines: history by IDENTITY, live by first-observed time ---

test("pre-stream ids stay in the history block, sorted by their own server time, before the backlog", () => {
  // The backlog: log lines the docker --tail burst delivers right after the
  // stream opens.
  const backlog = [
    { timeMS: 10_005, text: "backlog line 1" },
    { timeMS: 10_010, text: "backlog line 2" },
  ];
  // Two milestones that happened before the viewer ever opened the stream,
  // deliberately out of order here to prove they get sorted among themselves
  // (a same-clock comparison, server time to server time - never to the
  // backlog's browser-clock timestamps).
  const events = [
    event({ id: 2, created_at: new Date(9_000).toISOString(), params: { state: "starting" } }),
    event({ id: 1, created_at: new Date(8_000).toISOString(), params: { state: "pulling" } }),
  ];
  const merged = mergeLiveSystemLines(backlog, events, new Map());
  assert.deepEqual(
    merged.map((m) => (m.kind === "log" ? m.text : m.event.id)),
    [1, 2, "backlog line 1", "backlog line 2"],
    "history is identified by being known on the first merge, and reads in the order it happened, strictly before the backlog",
  );
});

test("a live milestone lands after the newest live line despite a +30s server clock skew, and stays there as more lines arrive", () => {
  let clock = 1_000;
  const now = () => clock;
  const receivedAt = new Map<number, number>();
  // timeline.events always carries the full accumulated list (history plus
  // any new milestones), never just a delta - mirrored here by re-passing
  // the same baseline event on every call.
  const baseline = event({ id: 1, created_at: new Date(500).toISOString(), params: { state: "pulling" } });

  // Establish history first (a baseline id known on the very first merge),
  // so the milestone introduced below is unambiguously "first seen later".
  mergeLiveSystemLines([], [baseline], receivedAt, now);

  clock = 1_010;
  let merged = mergeLiveSystemLines([{ timeMS: 1_010, text: "line 1" }], [baseline], receivedAt, now);
  assert.deepEqual(merged.map((m) => (m.kind === "log" ? m.text : "history")), ["history", "line 1"]);

  // A milestone is observed live, but the server's clock is 30s ahead, so its
  // own created_at claims a time far in the future - never consulted here.
  clock = 1_020;
  const aheadEvent = event({ id: 9, created_at: new Date(clock + 30_000).toISOString(), params: { state: "running" } });
  merged = mergeLiveSystemLines([{ timeMS: 1_010, text: "line 1" }], [baseline, aheadEvent], receivedAt, now);
  assert.deepEqual(merged.map((m) => (m.kind === "log" ? m.text : m.event.id === 1 ? "history" : "milestone")), ["history", "line 1", "milestone"]);

  // The exact failure this fixes: a later real log line must sort AFTER the
  // skewed milestone, not above it (which a claimed-time clamp would cause).
  clock = 1_030;
  merged = mergeLiveSystemLines(
    [{ timeMS: 1_010, text: "line 1" }, { timeMS: 1_030, text: "line 2" }],
    [baseline, aheadEvent],
    receivedAt, now,
  );
  assert.deepEqual(
    merged.map((m) => (m.kind === "log" ? m.text : m.event.id === 1 ? "history" : "milestone")),
    ["history", "line 1", "milestone", "line 2"],
    "a later live line must land after the +30s-skewed milestone, not before it",
  );
});

test("a live milestone lands after the newest live line despite a -30s server clock skew", () => {
  let clock = 5_000;
  const now = () => clock;
  const receivedAt = new Map<number, number>();
  const baseline = event({ id: 1, created_at: new Date(4_000).toISOString(), params: { state: "pulling" } });

  mergeLiveSystemLines([], [baseline], receivedAt, now);

  clock = 5_010;
  let merged = mergeLiveSystemLines([{ timeMS: 5_010, text: "line 1" }], [baseline], receivedAt, now);
  assert.deepEqual(merged.map((m) => (m.kind === "log" ? m.text : "history")), ["history", "line 1"]);

  // The server's clock lags by 30s: created_at claims a time before the
  // history event above, which a naive comparison would misfile as history.
  clock = 5_020;
  const laggingEvent = event({ id: 8, created_at: new Date(clock - 30_000).toISOString(), params: { state: "running" } });
  merged = mergeLiveSystemLines([{ timeMS: 5_010, text: "line 1" }], [baseline, laggingEvent], receivedAt, now);
  assert.deepEqual(
    merged.map((m) => (m.kind === "log" ? m.text : m.event.id === 1 ? "history" : "milestone")),
    ["history", "line 1", "milestone"],
    "a lagging server clock must not misfile a live milestone as history or sort it above an already-shown line",
  );
});

test("a live milestone's position is cached by id: it does not drift to a later slot on every re-render", () => {
  let clock = 100;
  const now = () => clock;
  const receivedAt = new Map<number, number>();
  const baseline = event({ id: 1, created_at: new Date(50).toISOString(), params: { state: "pulling" } });

  // Establish history so the next new id is unambiguously live.
  mergeLiveSystemLines([], [baseline], receivedAt, now);

  const liveEvent = event({ id: 3, created_at: new Date(90).toISOString(), params: { state: "running" } });
  clock = 100;
  const first = mergeLiveSystemLines([], [baseline, liveEvent], receivedAt, now);
  clock = 500; // time passes before the next render, e.g. a poll tick
  const second = mergeLiveSystemLines([{ timeMS: 200, text: "later line" }], [baseline, liveEvent], receivedAt, now);

  const firstMilestone = first.find((m) => m.kind === "system" && m.event.id === 3);
  const secondMilestone = second.find((m) => m.kind === "system" && m.event.id === 3);
  assert.equal(firstMilestone?.timeMS, secondMilestone?.timeMS, "the same event id keeps its first-seen time");
  // Because it was seen at clock 100, it still sorts before a line that
  // arrived at clock 200, even though "now" is 500 by the time of this call.
  assert.deepEqual(
    second.map((m) => (m.kind === "log" ? m.text : m.event.id === 3 ? "live-milestone" : "history")),
    ["history", "live-milestone", "later line"],
  );
});

test("an id that reappears after a reconnect (fresh receivedAt) is re-classified as history, not left live", () => {
  let clock = 1_000;
  const now = () => clock;
  const receivedAt = new Map<number, number>();

  // Session 1: establish history with id 1, then id 5 arrives live.
  mergeLiveSystemLines([], [event({ id: 1, created_at: new Date(500).toISOString(), params: { state: "pulling" } })], receivedAt, now);
  clock = 1_010;
  const liveInSession1 = event({ id: 5, created_at: new Date(1_005).toISOString(), params: { state: "running" } });
  const beforeReconnect = mergeLiveSystemLines([], [liveInSession1], receivedAt, now);
  assert.notEqual(beforeReconnect.find((m) => m.kind === "system" && m.event.id === 5)?.timeMS, 0, "id 5 is live in session 1, not history");

  // Reconnect: the component hands mergeLiveSystemLines a brand-new empty
  // Map. Both ids reappear (e.g. a refetch still returns them in the window).
  const afterReconnect = new Map<number, number>();
  clock = 2_000;
  const merged = mergeLiveSystemLines(
    [],
    [
      event({ id: 1, created_at: new Date(500).toISOString(), params: { state: "pulling" } }),
      event({ id: 5, created_at: new Date(1_005).toISOString(), params: { state: "running" } }),
    ],
    afterReconnect,
    now,
  );
  const id5 = merged.find((m) => m.kind === "system" && m.event.id === 5);
  assert.equal(id5?.timeMS, 0, "id 5 keeps a stable history position after reconnect, instead of carrying over its old live classification");
});

test("receivedAt is pruned to the ids present in the current call", () => {
  const receivedAt = new Map<number, number>();
  mergeLiveSystemLines(
    [],
    [event({ id: 1, created_at: new Date(500).toISOString() }), event({ id: 2, created_at: new Date(600).toISOString() })],
    receivedAt,
  );
  assert.deepEqual([...receivedAt.keys()].sort(), [1, 2]);

  // id 2 drops out of the window (e.g. paged out by a refetch); only id 1 remains.
  mergeLiveSystemLines([], [event({ id: 1, created_at: new Date(500).toISOString() })], receivedAt);
  assert.deepEqual([...receivedAt.keys()], [1], "an id no longer present must not linger in receivedAt forever");
});

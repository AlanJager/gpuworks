import type { InstanceEvent } from "../api";

// The instance panel used to have a separate events tab; state changes,
// schedule outcomes and errors now show up as system lines inside the log
// stream instead, ordered among the log lines. This module is the pure merge
// step: it owns ordering so that rule is unit-testable without mounting the
// log stream's EventSource plumbing.

export interface TimelineLogInput {
  timeMS: number;
  text: string;
  // level is the persisted row's classified severity ("warn"/"error"/...),
  // when known - carried through so the viewer can mute/highlight a line.
  // Live SSE lines have none: the raw stdout text carries no structured
  // level, only the agent's later classification of the persisted copy does.
  level?: string;
}

export interface TimelineLogLine {
  kind: "log";
  timeMS: number;
  text: string;
  level?: string;
}

export interface TimelineSystemLine {
  kind: "system";
  timeMS: number;
  event: InstanceEvent;
}

export type TimelineLine = TimelineLogLine | TimelineSystemLine;

// eventTimeMS reads the wall-clock time an event happened. An unparsable
// timestamp sorts to the very start rather than throwing, since a merge
// helper must not fail the whole log view over one bad row.
export function eventTimeMS(event: InstanceEvent): number {
  const parsed = Date.parse(event.created_at);
  return Number.isFinite(parsed) ? parsed : 0;
}

// stableSortByTime sorts ascending by timeMS, preserving the relative order
// of entries that share a timestamp (Array.prototype.sort is stable, but the
// index tiebreak below makes that guarantee explicit and independent of the
// engine's sort implementation).
function stableSortByTime<T extends { timeMS: number }>(items: T[]): T[] {
  return items
    .map((item, index) => ({ item, index }))
    .sort((a, b) => a.item.timeMS - b.item.timeMS || a.index - b.index)
    .map(({ item }) => item);
}

/**
 * mergeSystemLines interleaves log lines and instance events into one
 * chronological timeline, newest at the end (the direction the log view
 * scrolls to on follow). Both inputs are already on the same clock here (the
 * non-live, persisted case: agent-shipped log lines and server-stamped
 * events), so a plain time sort is correct - see mergeLiveSystemLines for the
 * live case, where that is not true.
 *
 * Ties (equal timeMS) keep log lines ahead of system lines, since that is the
 * order the two inputs are concatenated in before the stable sort - a log
 * burst that happens to share a millisecond with a milestone reads with its
 * own output first, then the milestone that followed it.
 */
export function mergeSystemLines(logLines: TimelineLogInput[], events: InstanceEvent[]): TimelineLine[] {
  const merged: TimelineLine[] = [
    ...logLines.map((line): TimelineLogLine => ({ kind: "log", timeMS: line.timeMS, text: line.text, level: line.level })),
    ...events.map((event): TimelineSystemLine => ({ kind: "system", timeMS: eventTimeMS(event), event })),
  ];
  return stableSortByTime(merged);
}

/**
 * mergeLiveSystemLines is mergeSystemLines' live-stream counterpart, and the
 * two must not be interchanged: a live log line is stamped with the browser's
 * receive clock (see InstanceLogStream), while an instance event carries the
 * server's own clock. This function never compares those two clocks against
 * each other, not even indirectly through a shared threshold - either clock
 * can run ahead of or behind the other by an unknown amount, and a comparison
 * across them is wrong regardless of which way it leans: sorting by "server
 * time is before this browser-clock cutoff" can misfile a genuinely live
 * event as history when the server lags, and clamping a live event's
 * position up to its own (skewed-ahead) server time pins it too far in the
 * apparent future - every real log line that arrives afterward then sorts
 * above it instead of below, for as long as the skew lasts.
 *
 * History is decided by IDENTITY instead, never by time:
 *
 *  - The first time this stream merges (receivedAt is still empty), every
 *    event id already present is history. Each is recorded with the sentinel
 *    0, and the whole history block therefore sorts before every log line and
 *    every live event - 0 is smaller than any receive-clock timestamp, live
 *    or historical text messages included. Within that block, entries are
 *    ordered by their own server timestamp (eventTimeMS below only ever
 *    compares one server timestamp to another, never to the browser clock),
 *    so history still reads in the order it actually happened.
 *  - Any event id not yet known after that first merge is new: it happened
 *    live, during this viewing session. It is placed at the moment this
 *    function first observed it (now()) - its own claimed time is never
 *    consulted for placement, so no amount of clock skew can move it.
 *    Re-seeing the same id later (a poll tick, a reload) reuses its
 *    remembered position instead of drifting forward on every render.
 *
 * receivedAt is mutated in place (by event id) so the caller can hold it in a
 * ref and get stable, monotonic ordering across re-renders without
 * persisting anything itself; resetting it to a fresh empty Map (e.g. on
 * reconnect) restarts the "first merge decides history" rule from scratch,
 * so an id that reappears after a reset is re-classified the same way rather
 * than keeping whatever bucket it happened to land in before the reset. It is
 * pruned to the ids present in this call so a long session does not grow it
 * forever.
 */
export function mergeLiveSystemLines(
  logLines: TimelineLogInput[],
  events: InstanceEvent[],
  receivedAt: Map<number, number>,
  now: () => number = Date.now,
): TimelineLine[] {
  // Same-clock comparison only (server timestamp vs. server timestamp), so
  // history reads in the order it happened once it is bucketed below.
  const chronological = [...events].sort((a, b) => eventTimeMS(a) - eventTimeMS(b));

  if (receivedAt.size === 0) {
    for (const event of chronological) {
      if (!receivedAt.has(event.id)) receivedAt.set(event.id, 0);
    }
  }

  const merged: TimelineLine[] = [
    ...logLines.map((line): TimelineLogLine => ({ kind: "log", timeMS: line.timeMS, text: line.text, level: line.level })),
    ...chronological.map((event): TimelineSystemLine => {
      let seenAt = receivedAt.get(event.id);
      if (seenAt === undefined) {
        seenAt = now();
        receivedAt.set(event.id, seenAt);
      }
      return { kind: "system", timeMS: seenAt, event };
    }),
  ];

  const currentIds = new Set(chronological.map((event) => event.id));
  for (const id of receivedAt.keys()) {
    if (!currentIds.has(id)) receivedAt.delete(id);
  }

  return stableSortByTime(merged);
}

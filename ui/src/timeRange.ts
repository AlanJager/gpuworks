// Single source of truth for the metric/log time-range selector shared by the
// deployment drawer: the quick options, their windows, the query step that
// keeps the returned point count bounded, and the polling cadence per window
// (a historical custom range is a one-shot query, so it never polls).
// RETENTION_MS must stay in sync with the server-side retention loop in
// internal/server/app/app.go (metrics.db and logs.db keep 7 days).

export type QuickRangeKey = "15m" | "1h" | "6h" | "1d" | "1w" | "1mo" | "1y";

export type TimeRangeSelection =
  | { kind: "quick"; key: QuickRangeKey }
  | { kind: "custom"; startMS: number; endMS: number };

export const RETENTION_MS = 7 * 24 * 60 * 60 * 1000;

export const RETENTION_WARNING_DAYS = RETENTION_MS / (24 * 60 * 60 * 1000);

type QuickRangeSpec = {
  key: QuickRangeKey;
  labelKey: string;
  ms: number;
  stepMS: number;
  pollMs: number;
};

// The labelKey indexes `timeRange.<key>` in the deployments namespace.
export const QUICK_RANGES: QuickRangeSpec[] = [
  { key: "15m", labelKey: "timeRange.15m", ms: 15 * 60 * 1000, stepMS: 10 * 1000, pollMs: 10 * 1000 },
  { key: "1h", labelKey: "timeRange.1h", ms: 60 * 60 * 1000, stepMS: 30 * 1000, pollMs: 15 * 1000 },
  { key: "6h", labelKey: "timeRange.6h", ms: 6 * 60 * 60 * 1000, stepMS: 2 * 60 * 1000, pollMs: 30 * 1000 },
  { key: "1d", labelKey: "timeRange.1d", ms: 24 * 60 * 60 * 1000, stepMS: 10 * 60 * 1000, pollMs: 60 * 1000 },
  { key: "1w", labelKey: "timeRange.1w", ms: 7 * 24 * 60 * 60 * 1000, stepMS: 60 * 60 * 1000, pollMs: 5 * 60 * 1000 },
  { key: "1mo", labelKey: "timeRange.1mo", ms: 30 * 24 * 60 * 60 * 1000, stepMS: 4 * 60 * 60 * 1000, pollMs: 10 * 60 * 1000 },
  { key: "1y", labelKey: "timeRange.1y", ms: 365 * 24 * 60 * 60 * 1000, stepMS: 24 * 60 * 60 * 1000, pollMs: 30 * 60 * 1000 },
];

// The quick ranges the scheduling page offers (its history and its activity
// chart): the one list both pickers show.
export const SCHEDULING_QUICK_RANGES: readonly QuickRangeKey[] = ["15m", "1h", "6h", "1d", "1w"];

export function quickRange(key: QuickRangeKey): QuickRangeSpec {
  const spec = QUICK_RANGES.find((entry) => entry.key === key);
  if (!spec) throw new Error(`unknown quick range: ${key}`);
  return spec;
}

export type ResolvedRange = {
  startMS: number;
  endMS: number;
  stepMS: number;
  pollMs: number;
  beyondRetention: boolean;
};

// resolveRange turns a selection into concrete query bounds. Quick ranges end
// at `now` so a polled window slides forward; a custom range is fixed, hence
// one-shot (pollMs 0).
export function resolveRange(selection: TimeRangeSelection, nowMS: number): ResolvedRange {
  if (selection.kind === "custom") {
    return {
      startMS: selection.startMS,
      endMS: selection.endMS,
      stepMS: customStepMS(selection.endMS - selection.startMS),
      pollMs: 0,
      beyondRetention: selection.startMS < nowMS - RETENTION_MS,
    };
  }
  const spec = quickRange(selection.key);
  return {
    startMS: nowMS - spec.ms,
    endMS: nowMS,
    stepMS: spec.stepMS,
    pollMs: spec.pollMs,
    beyondRetention: spec.ms > RETENTION_MS,
  };
}

function customStepMS(windowMS: number): number {
  const spec = [...QUICK_RANGES].reverse().find((entry) => windowMS >= entry.ms);
  return spec ? spec.stepMS : QUICK_RANGES[0].stepMS;
}

// Formats a millisecond duration as the step string the metrics API expects.
// Go's time.ParseDuration (server side) knows ns/us/ms/s/m/h only — no "d".
export function stepString(stepMS: number): string {
  if (stepMS % (60 * 60 * 1000) === 0) return `${stepMS / (60 * 60 * 1000)}h`;
  if (stepMS % (60 * 1000) === 0) return `${stepMS / (60 * 1000)}m`;
  return `${Math.round(stepMS / 1000)}s`;
}

export function isBeyondRetention(selection: TimeRangeSelection, nowMS: number): boolean {
  return resolveRange(selection, nowMS).beyondRetention;
}

export function selectionLabelKey(selection: TimeRangeSelection): string {
  return selection.kind === "custom" ? "timeRange.custom" : quickRange(selection.key).labelKey;
}

export function sameSelection(a: TimeRangeSelection, b: TimeRangeSelection): boolean {
  if (a.kind !== b.kind) return false;
  if (a.kind === "quick" && b.kind === "quick") return a.key === b.key;
  if (a.kind === "custom" && b.kind === "custom") return a.startMS === b.startMS && a.endMS === b.endMS;
  return false;
}

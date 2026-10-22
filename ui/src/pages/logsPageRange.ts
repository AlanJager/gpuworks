// LogsPage's time-range model: the preset dropdown, the fixed histogram
// bucket per preset, and the "custom" absolute window, picked in the range
// control or seeded on mount by a deep link (e.g. the service monitor page's
// per-line "→" link). Split out of
// LogsPage.tsx itself (which was pushing past the 600-line file-split rule)
// since this logic is self-contained and independently testable.

export const RANGE_KEYS = ["30m", "1h", "6h", "24h", "7d", "all"] as const;
export type PresetRangeKey = (typeof RANGE_KEYS)[number];
// "custom" is an absolute from/to (AbsoluteWindow below), chosen with the
// range control's date picker or seeded by a deep link; RANGE_KEYS are the
// presets ending now.
export type RangeKey = PresetRangeKey | "custom";

export const RANGE_MS: Record<Exclude<PresetRangeKey, "all">, number> = {
  "30m": 30 * 60_000,
  "1h": 3_600_000,
  "6h": 6 * 3_600_000,
  "24h": 24 * 3_600_000,
  "7d": 7 * 24 * 3_600_000,
};

// Fixed bucket size per range preset: epoch-aligned boundaries stay put
// across auto-refreshes, so bars do not jump while you watch.
const HISTOGRAM_BUCKET_MS: Record<PresetRangeKey, number> = {
  "30m": 60_000,
  "1h": 120_000,
  "6h": 720_000,
  "24h": 1_800_000,
  "7d": 10_800_000,
  "all": 10_800_000,
};

// customBucketMS mirrors the preset table above for a "custom" window: pick
// the smallest preset bucket whose own window still covers this span, so an
// absolute deep-linked range gets a bucket size in the same ballpark a
// preset of similar length would use, instead of a single hardcoded value.
function customBucketMS(windowMS: number): number {
  if (windowMS <= RANGE_MS["30m"]) return HISTOGRAM_BUCKET_MS["30m"];
  if (windowMS <= RANGE_MS["1h"]) return HISTOGRAM_BUCKET_MS["1h"];
  if (windowMS <= RANGE_MS["6h"]) return HISTOGRAM_BUCKET_MS["6h"];
  if (windowMS <= RANGE_MS["24h"]) return HISTOGRAM_BUCKET_MS["24h"];
  return HISTOGRAM_BUCKET_MS["7d"];
}

export interface AbsoluteWindow {
  from: number;
  to: number;
}

// initialAbsoluteWindow reads a deep link's absolute window (e.g. the
// service monitor page's per-line "→" link, /logs?...&from=<ms>&to=<ms>) -
// both bounds required, both numeric, and to > from, or the link is
// malformed and the page falls back to its normal preset default instead of
// querying a nonsense range.
export function initialAbsoluteWindow(searchParams: URLSearchParams): AbsoluteWindow | null {
  const from = Number(searchParams.get("from"));
  const to = Number(searchParams.get("to"));
  if (Number.isFinite(from) && from > 0 && Number.isFinite(to) && to > from) return { from, to };
  return null;
}

export function histogramBucketMS(range: RangeKey, absoluteWindow: AbsoluteWindow | null): number {
  if (range === "custom") return absoluteWindow ? customBucketMS(absoluteWindow.to - absoluteWindow.from) : HISTOGRAM_BUCKET_MS["24h"];
  return HISTOGRAM_BUCKET_MS[range];
}

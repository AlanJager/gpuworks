// The monitor pages' time range: a preset ending now ("1h"), or a range the
// operator picked, written "from~to" in Unix milliseconds so it travels in
// the URL and through every component as one opaque value. Only these
// helpers look inside it.

export type MonitorPreset = "15m" | "1h" | "6h" | "24h" | "168h";
export type MonitorRange = MonitorPreset | `${number}~${number}`;

export const MONITOR_PRESETS: MonitorPreset[] = ["15m", "1h", "6h", "24h", "168h"];

const PRESET_MS: Record<MonitorPreset, number> = {
  "15m": 15 * 60_000,
  "1h": 3_600_000,
  "6h": 6 * 3_600_000,
  "24h": 24 * 3_600_000,
  "168h": 168 * 3_600_000,
};

/** isPreset reports whether a value is one of the presets. */
export function isPreset(value: string | null | undefined): value is MonitorPreset {
  return !!value && (MONITOR_PRESETS as string[]).includes(value);
}

/** customRange writes a picked range. */
export function customRange(fromMS: number, toMS: number): MonitorRange {
  return `${Math.round(fromMS)}~${Math.round(toMS)}`;
}

/** pickedBounds reads a picked range, or null for a preset or a malformed value. */
export function pickedBounds(range: string | null | undefined): { from: number; to: number } | null {
  const m = /^(\d+)~(\d+)$/.exec(range ?? "");
  if (!m) return null;
  const from = Number(m[1]);
  const to = Number(m[2]);
  return from > 0 && to > from ? { from, to } : null;
}

/** rangeBounds is the window a range covers: a preset's ends at now. */
export function rangeBounds(range: MonitorRange, now = Date.now()): { from: number; to: number } {
  const picked = pickedBounds(range);
  if (picked) return picked;
  return { from: now - (PRESET_MS[range as MonitorPreset] ?? PRESET_MS["1h"]), to: now };
}

/** rangeParams are the query parameters the monitor API takes for a range. */
export function rangeParams(range: MonitorRange): Record<string, string> {
  const picked = pickedBounds(range);
  return picked ? { from: String(picked.from), to: String(picked.to) } : { range };
}

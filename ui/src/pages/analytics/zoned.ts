// Wall-clock arithmetic in the statistics time zone (settings: data
// retention): statistics count their days in that zone, whatever zone the
// browser is in, so the quick ranges and the labels are computed there too.

export interface ZonedParts {
  y: number;
  m: number;
  d: number;
  h: number;
  mi: number;
  s: number;
}

const formatters = new Map<string, Intl.DateTimeFormat>();

export function browserZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

/** usableZone is tz when the runtime knows it, else the browser's zone. */
export function usableZone(tz?: string): string {
  if (!tz) return browserZone();
  try {
    new Intl.DateTimeFormat("en-US", { timeZone: tz });
    return tz;
  } catch {
    return browserZone();
  }
}

function formatter(tz: string): Intl.DateTimeFormat {
  let f = formatters.get(tz);
  if (!f) {
    f = new Intl.DateTimeFormat("en-US", {
      timeZone: tz, hourCycle: "h23", year: "numeric", month: "numeric", day: "numeric", hour: "numeric", minute: "numeric", second: "numeric",
    });
    formatters.set(tz, f);
  }
  return f;
}

export function zonedParts(ms: number, tz: string): ZonedParts {
  const out: Record<string, number> = {};
  for (const part of formatter(tz).formatToParts(new Date(ms))) {
    if (part.type !== "literal") out[part.type] = Number(part.value);
  }
  return { y: out.year, m: out.month, d: out.day, h: out.hour % 24, mi: out.minute, s: out.second };
}

/** The zone's offset from UTC at ms, in ms. */
export function zoneOffsetMs(ms: number, tz: string): number {
  const p = zonedParts(ms, tz);
  return Date.UTC(p.y, p.m - 1, p.d, p.h, p.mi, p.s) - Math.floor(ms / 1000) * 1000;
}

/** zonedToMs is the instant at which the zone's clock reads y-m-d h:mi (a day or month past its end rolls over). */
export function zonedToMs(y: number, m: number, d: number, h: number, mi: number, tz: string): number {
  const wall = Date.UTC(y, m - 1, d, h, mi);
  let guess = wall;
  for (let i = 0; i < 3; i++) guess = wall - zoneOffsetMs(guess, tz);
  return guess;
}

export function startOfDay(ms: number, tz: string): number {
  const p = zonedParts(ms, tz);
  return zonedToMs(p.y, p.m, p.d, 0, 0, tz);
}

/** The start of the day n calendar days after the day ms is in. */
export function addDays(ms: number, n: number, tz: string): number {
  const p = zonedParts(ms, tz);
  return zonedToMs(p.y, p.m, p.d + n, 0, 0, tz);
}

function startOfMonth(ms: number, tz: string, delta = 0): number {
  const p = zonedParts(ms, tz);
  return zonedToMs(p.y, p.m + delta, 1, 0, 0, tz);
}

export const QUICK_KEYS = ["today", "yesterday", "7d", "30d", "month", "lastmonth"] as const;
export type QuickKey = (typeof QUICK_KEYS)[number];

export function isQuickKey(value: string | null | undefined): value is QuickKey {
  return !!value && (QUICK_KEYS as readonly string[]).includes(value);
}

/** quickRange is a quick range's [from, to) in the zone as of now; the ones that reach today end now. */
export function quickRange(key: QuickKey, now: number, tz: string): { from: number; to: number } {
  const today = startOfDay(now, tz);
  switch (key) {
    case "today": return { from: today, to: now };
    case "yesterday": return { from: addDays(now, -1, tz), to: today };
    case "7d": return { from: addDays(now, -6, tz), to: now };
    case "30d": return { from: addDays(now, -29, tz), to: now };
    case "month": return { from: startOfMonth(now, tz), to: now };
    case "lastmonth": return { from: startOfMonth(now, tz, -1), to: startOfMonth(now, tz) };
  }
}

const pad = (n: number) => String(n).padStart(2, "0");

/** A bucket's axis label: the hour (with its date when the range spans days) or the day. */
export function bucketLabel(ms: number, tz: string, granularity: "hour" | "day", withDate: boolean): string {
  const p = zonedParts(ms, tz);
  if (granularity === "day") return `${pad(p.m)}-${pad(p.d)}`;
  return `${withDate ? `${pad(p.m)}-${pad(p.d)} ` : ""}${pad(p.h)}:00`;
}

/** "10-22 08:30" in the zone. */
export function shortTime(ms: number, tz: string): string {
  const p = zonedParts(ms, tz);
  return `${pad(p.m)}-${pad(p.d)} ${pad(p.h)}:${pad(p.mi)}`;
}

/** "2026-10-22 08:30" in the zone. */
export function fullTime(ms: number, tz: string): string {
  const p = zonedParts(ms, tz);
  return `${p.y}-${pad(p.m)}-${pad(p.d)} ${pad(p.h)}:${pad(p.mi)}`;
}

/** "20261022" in the zone, for file names. */
export function compactDate(ms: number, tz: string): string {
  const p = zonedParts(ms, tz);
  return `${p.y}${pad(p.m)}${pad(p.d)}`;
}

/** True when the buckets do not all fall on one day of the zone. */
export function spansDays(buckets: number[], tz: string): boolean {
  if (buckets.length < 2) return false;
  return startOfDay(buckets[0], tz) !== startOfDay(buckets[buckets.length - 1], tz);
}

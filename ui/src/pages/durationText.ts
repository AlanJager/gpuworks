// The one wording of a span of seconds, in the deployments namespace's
// duration.* keys, for every page that words one (the timeline entries, the
// yield policy, the scheduling center's sharing stats). It takes the t of any
// namespace: the keys are named with theirs.

type Translate = (key: string, options: Record<string, unknown>) => string;

/**
 * How a span is worded:
 * - "rounded": a setting or a wait an event reports - whole days or hours when
 *   it divides evenly, else seconds under a minute and whole minutes above.
 * - "exact": a parameter an operator typed - whole days, hours or minutes when
 *   it divides evenly, else the exact seconds, so nothing typed is rounded.
 * - "elapsed": a measured span - seconds, minutes, then hours and minutes.
 */
export type DurationStyle = "rounded" | "exact" | "elapsed";

const key = (unit: string) => `deployments:duration.${unit}`;

export function durationText(t: Translate, seconds: number, style: DurationStyle = "rounded"): string {
  if (style === "elapsed") {
    if (seconds < 60) return t(key("seconds"), { value: Math.round(seconds) });
    const minutes = Math.round(seconds / 60);
    if (minutes < 60) return t(key("minutes"), { value: minutes });
    return t(key("hoursMinutes"), { hours: Math.floor(minutes / 60), minutes: minutes % 60 });
  }
  if (seconds >= 86400 && seconds % 86400 === 0) return t(key("days"), { value: seconds / 86400 });
  if (seconds >= 3600 && seconds % 3600 === 0) return t(key("hours"), { value: seconds / 3600 });
  if (style === "exact") {
    if (seconds >= 60 && seconds % 60 === 0) return t(key("minutes"), { value: seconds / 60 });
    return t(key("seconds"), { value: seconds });
  }
  if (seconds < 60) return t(key("seconds"), { value: Math.round(seconds) });
  return t(key("minutes"), { value: Math.round(seconds / 60) });
}

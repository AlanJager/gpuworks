import { QUICK_RANGES, quickRange, type QuickRangeKey, type TimeRangeSelection } from "../../timeRange";
import { rfc3339, type TimelineWindowOption } from "../../schedulingApi";

// The timeline's range picker is the shared TimeRangePicker. The windows it
// offers are the server's (the answer lists them); a quick range of the picker
// is the server window of the same length, so no key list lives here.

/** The quick ranges the picker offers: those the server lists a window of the same length for. */
export function offeredQuickRanges(windows: TimelineWindowOption[]): QuickRangeKey[] {
  return QUICK_RANGES.filter((range) => windows.some((window) => window.seconds * 1000 === range.ms)).map((range) => range.key);
}

/** The server window a quick range asks for, undefined when the server lists none of that length. */
export function windowKeyOf(key: QuickRangeKey, windows: TimelineWindowOption[]): string | undefined {
  const ms = quickRange(key).ms;
  return windows.find((window) => window.seconds * 1000 === ms)?.key;
}

/** The picker's selection for the window the server answered (its default until another is picked). */
export function selectionOfWindow(answered: { seconds: number }): TimeRangeSelection | null {
  const spec = QUICK_RANGES.find((range) => range.ms === answered.seconds * 1000);
  return spec ? { kind: "quick", key: spec.key } : null;
}

/** What the timeline is asked for: the server's default, one of its windows, or a custom range. */
export type TimelineAsk = { window?: string } | { since: string; until: string };

/** The ask for a selection; no selection (or a quick range the server does not list) leaves the window to the server. */
export function askOf(range: TimeRangeSelection | undefined, windows: TimelineWindowOption[]): TimelineAsk {
  if (!range) return { window: undefined };
  if (range.kind === "custom") return { since: rfc3339(range.startMS), until: rfc3339(range.endMS) };
  return { window: windowKeyOf(range.key, windows) };
}

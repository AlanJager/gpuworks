import type { TimelineDeployment, TimelineHours, TimelineSegment, TimelineState } from "../../schedulingApi";
import type { YieldPolicyName } from "../../yieldPolicyApi";
import { colors } from "../../theme/tokens";

// Geometry of the residency timeline. Every x is a position on a track that is
// CHART_WIDTH units wide and spans the window exactly, so a block's width is
// its share of the window and a tick sits where its hour does.
export const CHART_WIDTH = 1000;
// MIN_BLOCK keeps a very short stretch (a one-minute switch in 24 h) visible;
// it is the only place the drawing departs from the scale.
const MIN_BLOCK = 1.5;

export interface Span {
  fromMs: number;
  toMs: number;
}

export function windowSpan(window: { from: string; to: string }): Span {
  return { fromMs: Date.parse(window.from), toMs: Date.parse(window.to) };
}

/** x of an instant on the track, clamped into the window. */
export function xOf(ms: number, span: Span): number {
  const width = Math.max(span.toMs - span.fromMs, 1);
  return Math.min(Math.max((ms - span.fromMs) / width, 0), 1) * CHART_WIDTH;
}

export function blockOf(segment: TimelineSegment, span: Span): { x: number; width: number } {
  const x = xOf(Date.parse(segment.from), span);
  const end = xOf(Date.parse(segment.to), span);
  const width = Math.max(end - x, MIN_BLOCK);
  return { x: Math.min(x, CHART_WIDTH - width), width };
}

export interface Tick {
  ms: number;
  x: number;
}

/**
 * Ticks at the local whole hours inside the window, about six of them: every
 * hour for 6 h, every 4 h for 24 h.
 */
export function ticksOf(span: Span, hours: TimelineHours): Tick[] {
  const step = Math.max(1, Math.round(hours / 6));
  const first = new Date(span.fromMs);
  first.setMinutes(0, 0, 0);
  const out: Tick[] = [];
  for (let at = new Date(first); at.getTime() <= span.toMs; at.setHours(at.getHours() + 1)) {
    if (at.getTime() < span.fromMs || at.getHours() % step !== 0) continue;
    out.push({ ms: at.getTime(), x: xOf(at.getTime(), span) });
  }
  return out;
}

/** Fill of each state, from the theme's chart colours. */
export const stateColor: Record<TimelineState, string> = {
  awake: colors.chartPrimary,
  asleep: colors.chartQuaternary,
  switching: colors.chartSecondary,
  starting: colors.chartTeal,
};

export const stateOrder: TimelineState[] = ["awake", "asleep", "switching", "starting"];

export interface Lane {
  deploymentID: number;
  deployment: string;
  policy: YieldPolicyName;
  /** The deployment has been deleted since (the stats say so). */
  deleted: boolean;
  segments: TimelineSegment[];
}

/**
 * One lane per deployment that held the card, in the order they first held it.
 * A deployment missing from the stats is a resident one: the stats list only
 * the deployments that share cards. A deleted one is marked as its stats row
 * marks it.
 */
export function lanesOf(segments: TimelineSegment[], stats: TimelineDeployment[]): Lane[] {
  const rows = new Map(stats.map((row) => [row.deployment_id, row]));
  const lanes = new Map<number, Lane>();
  for (const segment of segments) {
    let lane = lanes.get(segment.deployment_id);
    if (!lane) {
      const row = rows.get(segment.deployment_id);
      lane = { deploymentID: segment.deployment_id, deployment: segment.deployment, policy: row?.yield_policy || "never", deleted: row?.deployment_deleted ?? false, segments: [] };
      lanes.set(segment.deployment_id, lane);
    }
    lane.segments.push(segment);
  }
  return [...lanes.values()];
}

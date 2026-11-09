import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode, type RefObject } from "react";
import { createPortal } from "react-dom";
import { Card, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { api, usePolling, type MonitorReplica } from "../../api";
import type { MonitorRequest, MonitorRequests } from "../../monitorRequestsApi";
import { colors } from "../../theme/tokens";
import InfoTitle from "./InfoTitle";
import { callerName, SEGMENT_STYLE } from "./RequestRow";
import { formatMS } from "./requestShape";
import { replicaLabels } from "./RequestView";

const POLL_MS = 2000;
// The most in-flight requests one poll draws; the phase counts cover all.
const LIMIT = 500;
// A request thinking or answering that has sent nothing new for this long
// is stalled.
export const STALL_MS = 2000;

type Lane = "waiting" | "thinking" | "answering";
const LANES: Lane[] = ["waiting", "thinking", "answering"];

// The swimlane dots wear their phase's colour; the unsplit wait is a hatch
// on the bars, a neutral grey as a dot.
const LANE_FILL: Record<Lane, string> = {
  waiting: colors.weak,
  thinking: SEGMENT_STYLE.thinking,
  answering: SEGMENT_STYLE.answering,
};

function laneOf(r: MonitorRequest): Lane | null {
  return r.phase === "waiting" || r.phase === "thinking" || r.phase === "answering" ? r.phase : null;
}

// stalledFor is how long a request in a streaming phase has sent nothing
// new; null for one still waiting for its first token.
export function stalledFor(r: MonitorRequest): number | null {
  if ((r.phase !== "thinking" && r.phase !== "answering") || r.age_ms == null || r.last_chunk_ms == null) return null;
  return Math.max(0, r.age_ms - r.last_chunk_ms);
}

// LiveRequestsOverview is the second row of the service page: how many
// requests are in flight in each phase, and every one of them as a dot on
// a lane per phase by how long it has been in flight, so a queue building
// up and a single stuck request both show at a glance at any concurrency.
export default function LiveRequestsOverview({ deploymentId, replicas }: { deploymentId: number; replicas: MonitorReplica[] }) {
  const fetcher = useCallback(() => api.monitorRequests(deploymentId, { view: "live", limit: LIMIT }), [deploymentId]);
  const { data } = usePolling<MonitorRequests>(fetcher, POLL_MS, [deploymentId]);
  return (
    <div className="monitor-service-row2">
      <Card size="small">
        <PhaseBar data={data} />
      </Card>
      <Card size="small">
        <Swimlane data={data} replicas={replicas} />
      </Card>
    </div>
  );
}

function PhaseBar({ data }: { data: MonitorRequests | null }) {
  const { t } = useTranslation("monitoring");
  const counts = data?.counts;
  const total = counts ? counts.waiting + counts.thinking + counts.answering : 0;
  const longestWait = useMemo(
    () => (data?.requests ?? []).filter((r) => r.phase === "waiting").reduce((m, r) => Math.max(m, r.age_ms ?? 0), 0),
    [data],
  );
  const stalled = useMemo(() => (data?.requests ?? []).filter((r) => (stalledFor(r) ?? 0) > STALL_MS).length, [data]);
  return (
    <div>
      <InfoTitle title={t("live.title")} help={t("live.help")} />
      <div className="monitor-latency-total">
        <span className="monitor-latency-mean">{counts ? total : "—"}</span>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {t("live.inFlight")}
        </Typography.Text>
      </div>
      <div className="monitor-latency-bar" role="list" aria-label={t("requests.counts")}>
        {counts &&
          LANES.map((lane) =>
            counts[lane] > 0 ? (
              <span
                key={lane}
                role="listitem"
                data-kind={lane}
                aria-label={`${t(`requests.phase.${lane}`)} ${counts[lane]}`}
                style={{ flexGrow: counts[lane], background: SEGMENT_STYLE[lane] }}
              />
            ) : null,
          )}
        {counts && total === 0 && <span style={{ flexGrow: 1, background: colors.borderSoft }} />}
      </div>
      <div className="monitor-latency-legend">
        {LANES.map((lane) => (
          <span key={lane}>
            <i style={{ background: SEGMENT_STYLE[lane] }} />
            <span style={{ color: colors.muted }}>{t(`requests.phase.${lane}`)}</span>
            <strong>{counts ? counts[lane] : "—"}</strong>
          </span>
        ))}
      </div>
      <div className="monitor-live-notes">
        <span>{t("live.longestWait", { time: longestWait > 0 ? formatMS(longestWait) : "—" })}</span>
        <span style={stalled > 0 ? { color: colors.dangerText } : undefined}>{t("live.stalled", { count: stalled, seconds: STALL_MS / 1000 })}</span>
      </div>
    </div>
  );
}

// Plot geometry, in pixels: the plot is drawn at its card's measured width
// (FALLBACK_W until measured), so its height is fixed and small whatever the
// screen is wide.
const FALLBACK_W = 640;
const LANE_H = 30;
const TOP = 3;
const LEFT = 62;
const RIGHT = 10;
const AXIS_H = 16;
const H = TOP + LANES.length * LANE_H + AXIS_H;
const DOT_R = 3.5;
const MIN_MS = 100;
const TICKS_MS = [100, 1000, 2000, 5000, 10_000, 30_000, 60_000, 120_000, 300_000, 600_000, 1_800_000, 3_600_000];

// useWidth is the element's current content width, kept up to date as it
// resizes; 0 until it is measured (and always in a DOM without layout).
function useWidth<T extends HTMLElement>(): [RefObject<T | null>, number] {
  const ref = useRef<T>(null);
  const [width, setWidth] = useState(0);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const measure = () => setWidth(Math.round(el.getBoundingClientRect().width));
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  return [ref, width];
}

// jitter spreads a lane's dots vertically by a stable hash of the request
// id, so a dot keeps its place from one poll to the next.
function jitter(id: string): number {
  let h = 2166136261;
  for (let i = 0; i < id.length; i++) h = Math.imul(h ^ id.charCodeAt(i), 16777619);
  return ((h >>> 0) % 1000) / 1000 - 0.5;
}

// Anchor is the hovered dot's box in viewport pixels.
type Anchor = { left: number; right: number; top: number; bottom: number };

function anchorOf(el: Element): Anchor {
  const { left, right, top, bottom } = el.getBoundingClientRect();
  return { left, right, top, bottom };
}

const TIP_GAP = 6;
const TIP_MARGIN = 8;

// SwimlaneTip is the hover box of a dot. It is drawn in the page body, not in
// the card (whose body clips what overflows it), fixed beside the dot: below
// it, or above when there is no room below, and kept inside the viewport.
// It goes away with a scroll, as the dot moves from under it.
function SwimlaneTip({ anchor, children }: { anchor: Anchor; children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null);
  const [pos, setPos] = useState<{ left: number; top: number } | null>(null);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const { width, height } = el.getBoundingClientRect();
    const vw = window.innerWidth;
    const vh = window.innerHeight;
    const mid = (anchor.left + anchor.right) / 2;
    const left = Math.max(TIP_MARGIN, Math.min(mid - width / 2, vw - width - TIP_MARGIN));
    const below = anchor.bottom + TIP_GAP;
    const top = below + height + TIP_MARGIN <= vh ? below : Math.max(TIP_MARGIN, anchor.top - TIP_GAP - height);
    setPos({ left, top });
  }, [anchor]);
  return createPortal(
    <div ref={ref} className="monitor-swimlane-tip" role="tooltip" style={{ left: pos?.left ?? 0, top: pos?.top ?? 0, visibility: pos ? "visible" : "hidden" }}>
      {children}
    </div>,
    document.body,
  );
}

function Swimlane({ data, replicas }: { data: MonitorRequests | null; replicas: MonitorReplica[] }) {
  const { t } = useTranslation("monitoring");
  const labels = useMemo(() => replicaLabels(replicas), [replicas]);
  const [hover, setHover] = useState<{ r: MonitorRequest; anchor: Anchor } | null>(null);
  const [plotRef, measured] = useWidth<HTMLDivElement>();
  const hovering = hover !== null;
  useEffect(() => {
    if (!hovering) return;
    const hide = () => setHover(null);
    window.addEventListener("scroll", hide, true);
    return () => window.removeEventListener("scroll", hide, true);
  }, [hovering]);
  const W = measured >= 240 ? measured : FALLBACK_W;
  const requests = data?.requests ?? [];
  const counts = data?.counts;
  const total = counts ? counts.waiting + counts.thinking + counts.answering : 0;
  const maxAge = requests.reduce((m, r) => Math.max(m, r.age_ms ?? 0), 0);
  const maxMS = Math.max(60_000, maxAge * 1.2);
  const x = (ms: number) => LEFT + ((Math.log10(Math.max(ms, MIN_MS)) - Math.log10(MIN_MS)) / (Math.log10(maxMS) - Math.log10(MIN_MS))) * (W - LEFT - RIGHT);
  const laneY = (lane: Lane) => TOP + LANES.indexOf(lane) * LANE_H + LANE_H / 2;
  const ticks = TICKS_MS.filter((ms) => ms <= maxMS);
  const summary = t("live.swimlaneSummary", { count: total, drawn: requests.length });

  return (
    <div>
      <div className="monitor-swimlane-head">
        <InfoTitle title={t("live.swimlane")} help={t("live.swimlaneHelp")} />
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {requests.length < total ? t("live.drawnLongest", { drawn: requests.length, count: total }) : summary}
        </Typography.Text>
      </div>
      <div ref={plotRef} className="monitor-swimlane-plot">
      <svg viewBox={`0 0 ${W} ${H}`} width={W} height={H} role="img" aria-label={summary} style={{ display: "block" }} onMouseLeave={() => setHover(null)}>
        {LANES.map((lane) => (
          <g key={lane}>
            <rect x={LEFT} y={TOP + LANES.indexOf(lane) * LANE_H + 2} width={W - LEFT - RIGHT} height={LANE_H - 4} rx={2} fill={colors.borderSoft} />
            <text x={LEFT - 8} y={laneY(lane) + 3.5} textAnchor="end" fontSize={10} fill={colors.muted}>
              {t(`requests.phase.${lane}`)}
            </text>
          </g>
        ))}
        {ticks.map((ms) => (
          <g key={ms}>
            <line x1={x(ms)} x2={x(ms)} y1={TOP} y2={TOP + LANES.length * LANE_H} stroke={colors.chartGrid} strokeWidth={1} />
            <text x={x(ms)} y={H - 4} textAnchor="middle" fontSize={9} fill={colors.chartTick}>
              {formatMS(ms)}
            </text>
          </g>
        ))}
        {requests.map((r) => {
          const lane = laneOf(r);
          if (!lane || r.age_ms == null) return null;
          const cx = x(r.age_ms);
          const cy = laneY(lane) + jitter(r.id) * (LANE_H - 12);
          const stalled = (stalledFor(r) ?? 0) > STALL_MS;
          return (
            <g key={r.id} data-request={r.id} data-lane={lane} data-stalled={stalled || undefined}>
              <circle
                cx={cx}
                cy={cy}
                r={DOT_R}
                fill={LANE_FILL[lane]}
                stroke={stalled ? colors.dangerText : colors.white}
                strokeWidth={stalled ? 1.75 : 1.25}
              />
              {/* The hit target is larger than the dot. */}
              <circle cx={cx} cy={cy} r={7} fill="transparent" onMouseEnter={(e) => setHover({ r, anchor: anchorOf(e.currentTarget) })} onMouseLeave={() => setHover(null)} />
            </g>
          );
        })}
      </svg>
      {hover && (
        <SwimlaneTip anchor={hover.anchor}>
          <strong>{callerName(hover.r, t)}</strong>
          <span>
            {t(`requests.phase.${hover.r.phase}`)} · {formatMS(hover.r.age_ms ?? 0)}
          </span>
          {labels.get(hover.r.instance_uuid) && (
            <span>{t("requests.replica", { n: labels.get(hover.r.instance_uuid)!.n, node: labels.get(hover.r.instance_uuid)!.node })}</span>
          )}
          {hover.r.events > 0 && <span>{t("requests.chunks", { count: hover.r.events })}</span>}
          {(stalledFor(hover.r) ?? 0) > STALL_MS && <span style={{ color: colors.dangerText }}>{t("live.stalledFor", { time: formatMS(stalledFor(hover.r)!) })}</span>}
        </SwimlaneTip>
      )}
      </div>
    </div>
  );
}

import { useId, useState, type KeyboardEvent } from "react";
import { Tag, Typography } from "antd";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { SchedulingTimeline, TimelineDeployment, TimelineGPU, TimelineNode, TimelineSegment } from "../../schedulingApi";
import { formatTime } from "../../i18n/datetime";
import { colors } from "../../theme/tokens";
import { CHART_WIDTH, LANE_HEIGHT, blockOf, lanesOf, stateColor, stateOrder, tickLabel, ticksOf, windowSpan, xOf, type Lane, type Span } from "./timelineChart";
import { BoundaryLine, HatchPattern, summaryText } from "./TimelineSummary";

/** stateText words a segment's state, with the sleep level when asleep. */
export function stateText(t: TFunction<"scheduling">, segment: Pick<TimelineSegment, "state" | "level">): string {
  return segment.state === "asleep" && segment.level ? t("timeline.state.asleepLevel", { level: segment.level }) : t(`timeline.state.${segment.state}`);
}

/**
 * TimelineLegend names the four state colours once for the whole chart, and,
 * when some card's earlier time is summarized, the hatch that marks it.
 */
export function TimelineLegend({ summarized }: { summarized: boolean }) {
  const { t } = useTranslation("scheduling");
  return (
    <ul className="scheduling-legend" aria-label={t("timeline.legend")}>
      {stateOrder.map((state) => (
        <li key={state}>
          <span className="scheduling-swatch" style={{ background: stateColor[state] }} aria-hidden="true" />
          {t(`timeline.state.${state}`)}
        </li>
      ))}
      {summarized && (
        <li>
          <span className="scheduling-swatch timeline-summary-swatch" aria-hidden="true" />
          {t("timeline.summary.legend")}
        </li>
      )}
    </ul>
  );
}

/** TimelineNodes draws every node's cards as lanes on one time axis. */
export function TimelineNodes({ timeline, gpuNames }: { timeline: SchedulingTimeline; gpuNames: Map<string, string> }) {
  const span = windowSpan(timeline.window);
  return (
    <div className="scheduling-timeline-nodes">
      {timeline.nodes.map((node) => (
        <TimelineNodeBlock key={node.uuid} node={node} span={span} stats={timeline.deployments} gpuNames={gpuNames} />
      ))}
    </div>
  );
}

function TimelineNodeBlock({ node, span, stats, gpuNames }: { node: TimelineNode; span: Span; stats: TimelineDeployment[]; gpuNames: Map<string, string> }) {
  const { t } = useTranslation("scheduling");
  const ticks = ticksOf(span);
  return (
    <section className="scheduling-node" aria-label={t("timeline.nodeAria", { node: node.name })}>
      <Typography.Text strong>{node.name}</Typography.Text>
      {node.gpus.length === 0 && <Typography.Paragraph type="secondary" style={{ margin: "8px 0 0" }}>{t("nodes.noGpus")}</Typography.Paragraph>}
      {node.gpus.length > 0 && (
        <div className="timeline-axis" aria-hidden="true">
          <span />
          <div className="timeline-axis-ticks">
            {ticks.map((tick) => (
              <span key={tick.ms} style={{ left: `${(tick.x / CHART_WIDTH) * 100}%` }}>
                {tickLabel(tick.ms, span)}
              </span>
            ))}
          </div>
        </div>
      )}
      {node.gpus.map((gpu) => (
        <GpuLanes key={gpu.index} node={node} gpu={gpu} gpuName={gpuNames.get(`${node.uuid}/${gpu.index}`)} span={span} ticks={ticks.map((tick) => tick.x)} stats={stats} />
      ))}
    </section>
  );
}

function GpuLanes({ node, gpu, gpuName, span, ticks, stats }: { node: TimelineNode; gpu: TimelineGPU; gpuName?: string; span: Span; ticks: number[]; stats: TimelineDeployment[] }) {
  const { t } = useTranslation("scheduling");
  const lanes = lanesOf(gpu.segments, stats, gpu.summarized_before ? gpu.summary : undefined);
  // Where the earlier time is summarized up to, when it is: a line on every lane marks it.
  const boundaryX = gpu.summarized_before && gpu.summary?.length ? xOf(Date.parse(gpu.summarized_before), span) : undefined;
  return (
    <div className="scheduling-gpu" role="group" aria-label={t("timeline.gpuAria", { node: node.name, index: gpu.index })}>
      <div className="scheduling-gpu-head">
        <Typography.Text>{gpuName ? t("nodes.gpuLabel", { index: gpu.index, name: gpuName }) : t("timeline.gpuPlain", { index: gpu.index })}</Typography.Text>
        {gpu.summarized_before ? (
          <Tag color="gold">{t("timeline.summarized", { time: formatTime(gpu.summarized_before) })}</Tag>
        ) : gpu.truncated && <Tag color="gold">{t("timeline.truncated")}</Tag>}
      </div>
      {lanes.length === 0 && <Typography.Text type="secondary" className="timeline-none">{t("timeline.noneOnCard")}</Typography.Text>}
      {lanes.map((lane) => <LaneRow key={lane.deploymentID} lane={lane} span={span} ticks={ticks} boundaryX={boundaryX} />)}
    </div>
  );
}

/** What a lane's tooltip shows: where the block it is for sits and its words. */
interface Hover {
  block: { x: number; width: number };
  text: string;
}

function LaneRow({ lane, span, ticks, boundaryX }: { lane: Lane; span: Span; ticks: number[]; boundaryX?: number }) {
  const { t } = useTranslation("scheduling");
  const { t: tDeploy } = useTranslation("deployments");
  const hatch = useId().replace(/:/g, "");
  const [active, setActive] = useState<Hover | null>(null);
  const describe = (segment: TimelineSegment) =>
    t("timeline.segmentText", { deployment: segment.deployment, state: stateText(t, segment), from: formatTime(segment.from), to: formatTime(segment.to) });
  // A block describes itself to assistive tech and, on hover and keyboard focus, in a tooltip.
  const hoverable = (block: Hover["block"], text: string) => ({
    tabIndex: 0,
    role: "img",
    "aria-label": text,
    onMouseEnter: () => setActive({ block, text }),
    onMouseLeave: () => setActive(null),
    onFocus: () => setActive({ block, text }),
    onBlur: () => setActive(null),
    onKeyDown: (event: KeyboardEvent<SVGRectElement>) => { if (event.key === "Escape") setActive(null); },
  });
  return (
    <div className="timeline-lane">
      <div className="timeline-lane-label">
        {lane.deleted ? (
          <Typography.Text type="secondary">{t("history.deleted", { name: lane.deployment })}</Typography.Text>
        ) : (
          <>
            <Link to={`/deployments?deployment=${lane.deploymentID}`}>{lane.deployment}</Link>
            <span className="timeline-level">{tDeploy(`yieldPolicy.policy.name.${lane.policy}`)}</span>
          </>
        )}
      </div>
      <div className="timeline-track">
        <svg viewBox={`0 0 ${CHART_WIDTH} ${LANE_HEIGHT}`} preserveAspectRatio="none" role="group" aria-label={t("timeline.laneAria", { deployment: lane.deployment })}>
          {lane.summary.length > 0 && <defs><HatchPattern id={hatch} /></defs>}
          <rect x={0} y={0} width={CHART_WIDTH} height={LANE_HEIGHT} fill={colors.chartGrid} opacity={0.5} />
          {ticks.map((x) => <line key={x} x1={x} x2={x} y1={0} y2={LANE_HEIGHT} stroke={colors.white} strokeWidth={1} vectorEffect="non-scaling-stroke" />)}
          {lane.summary.map(({ slice, deployment }) => {
            const block = blockOf(slice, span);
            return (
              <g key={slice.from}>
                <rect
                  className="timeline-block timeline-summary-block"
                  x={block.x}
                  y={2}
                  width={block.width}
                  height={LANE_HEIGHT - 4}
                  fill={stateColor[deployment.state]}
                  {...hoverable(block, summaryText(t, slice, deployment))}
                />
                <rect x={block.x} y={2} width={block.width} height={LANE_HEIGHT - 4} fill={`url(#${hatch})`} pointerEvents="none" aria-hidden="true" />
              </g>
            );
          })}
          {lane.segments.map((segment) => {
            const block = blockOf(segment, span);
            return (
              <rect
                key={`${segment.instance}-${segment.from}`}
                className="timeline-block"
                x={block.x}
                y={2}
                width={block.width}
                height={LANE_HEIGHT - 4}
                rx={1}
                fill={stateColor[segment.state]}
                {...hoverable(block, describe(segment))}
              />
            );
          })}
          {boundaryX !== undefined && <BoundaryLine x={boundaryX} />}
        </svg>
        {active && (
          <div role="tooltip" className="timeline-tooltip" style={{ left: `${((active.block.x + active.block.width / 2) / CHART_WIDTH) * 100}%` }}>
            {active.text}
          </div>
        )}
      </div>
    </div>
  );
}

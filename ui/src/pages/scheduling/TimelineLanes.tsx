import { useState } from "react";
import { Tag, Typography } from "antd";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { SchedulingTimeline, TimelineDeployment, TimelineGPU, TimelineNode, TimelineSegment } from "../../schedulingApi";
import { formatTime } from "../../i18n/datetime";
import { colors } from "../../theme/tokens";
import { CHART_WIDTH, blockOf, lanesOf, stateColor, stateOrder, tickLabel, ticksOf, windowSpan, type Lane, type Span } from "./timelineChart";

const LANE_HEIGHT = 18;

/** stateText words a segment's state, with the sleep level when asleep. */
export function stateText(t: TFunction<"scheduling">, segment: Pick<TimelineSegment, "state" | "level">): string {
  return segment.state === "asleep" && segment.level ? t("timeline.state.asleepLevel", { level: segment.level }) : t(`timeline.state.${segment.state}`);
}

/** TimelineLegend names the four state colours once for the whole chart. */
export function TimelineLegend() {
  const { t } = useTranslation("scheduling");
  return (
    <ul className="scheduling-legend" aria-label={t("timeline.legend")}>
      {stateOrder.map((state) => (
        <li key={state}>
          <span className="scheduling-swatch" style={{ background: stateColor[state] }} aria-hidden="true" />
          {t(`timeline.state.${state}`)}
        </li>
      ))}
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
  const lanes = lanesOf(gpu.segments, stats);
  return (
    <div className="scheduling-gpu" role="group" aria-label={t("timeline.gpuAria", { node: node.name, index: gpu.index })}>
      <div className="scheduling-gpu-head">
        <Typography.Text>{gpuName ? t("nodes.gpuLabel", { index: gpu.index, name: gpuName }) : t("timeline.gpuPlain", { index: gpu.index })}</Typography.Text>
        {gpu.truncated && <Tag color="gold">{t("timeline.truncated")}</Tag>}
      </div>
      {lanes.length === 0 && <Typography.Text type="secondary" className="timeline-none">{t("timeline.noneOnCard")}</Typography.Text>}
      {lanes.map((lane) => <LaneRow key={lane.deploymentID} lane={lane} span={span} ticks={ticks} />)}
    </div>
  );
}

function LaneRow({ lane, span, ticks }: { lane: Lane; span: Span; ticks: number[] }) {
  const { t } = useTranslation("scheduling");
  const { t: tDeploy } = useTranslation("deployments");
  const [active, setActive] = useState<TimelineSegment | null>(null);
  const activeBlock = active ? blockOf(active, span) : null;
  const describe = (segment: TimelineSegment) =>
    t("timeline.segmentText", { deployment: segment.deployment, state: stateText(t, segment), from: formatTime(segment.from), to: formatTime(segment.to) });
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
          <rect x={0} y={0} width={CHART_WIDTH} height={LANE_HEIGHT} fill={colors.chartGrid} opacity={0.5} />
          {ticks.map((x) => <line key={x} x1={x} x2={x} y1={0} y2={LANE_HEIGHT} stroke={colors.white} strokeWidth={1} vectorEffect="non-scaling-stroke" />)}
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
                tabIndex={0}
                role="img"
                aria-label={describe(segment)}
                onMouseEnter={() => setActive(segment)}
                onMouseLeave={() => setActive(null)}
                onFocus={() => setActive(segment)}
                onBlur={() => setActive(null)}
                onKeyDown={(event) => { if (event.key === "Escape") setActive(null); }}
              />
            );
          })}
        </svg>
        {active && activeBlock && (
          <div role="tooltip" className="timeline-tooltip" style={{ left: `${((activeBlock.x + activeBlock.width / 2) / CHART_WIDTH) * 100}%` }}>
            {describe(active)}
          </div>
        )}
      </div>
    </div>
  );
}

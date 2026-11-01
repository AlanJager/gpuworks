import { useId, useState } from "react";
import { Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { TimelineGPU, TimelineSummary } from "../../schedulingApi";
import { formatTime } from "../../i18n/datetime";
import { colors } from "../../theme/tokens";
import { durationText } from "../durationText";
import { CHART_WIDTH, blockOf, deploymentColor, xOf, type Span } from "./timelineChart";

const LANE_HEIGHT = 18;

/**
 * summaryText words one summarized slice from the numbers the server counted:
 * its time, how often the card changed hands and, per deployment, how long it
 * served, slept, switched and started (the states it spent no time in are left
 * out).
 */
export function summaryText(t: TFunction<"scheduling">, slice: TimelineSummary): string {
  const head = t("timeline.summary.head", { from: formatTime(slice.from), to: formatTime(slice.to), switches: t("timeline.summary.switches", { count: slice.switches }) });
  const parts = slice.deployments.flatMap((d) => {
    const spent = (["awake", "asleep", "switching", "starting"] as const)
      .map((state) => [state, d[`${state}_seconds`]] as const)
      .filter(([, seconds]) => seconds > 0)
      .map(([state, seconds]) => t(`timeline.summary.${state}`, { duration: durationText(t, seconds, "elapsed") }));
    return spent.length === 0 ? [] : [t("timeline.summary.deployment", { deployment: d.deployment, parts: spent.join(t("timeline.summary.partSeparator")) })];
  });
  return [head, ...parts].join(" · ");
}

/** BoundaryLine is the dashed line where summarizing ends, drawn across a lane. */
export function BoundaryLine({ x }: { x: number }) {
  return <line x1={x} x2={x} y1={0} y2={LANE_HEIGHT} stroke={colors.chartSlate} strokeWidth={1.5} strokeDasharray="3 2" vectorEffect="non-scaling-stroke" aria-hidden="true" />;
}

/**
 * SummaryLane is the lane of a card's summarized earlier time: one hatched
 * block per slice, in the colour of the deployment the server names as its
 * dominant one, with a dashed line where summarizing ends. A block describes
 * itself on hover and keyboard focus like a segment does.
 */
export function SummaryLane({ gpu, span, ticks }: { gpu: TimelineGPU; span: Span; ticks: number[] }) {
  const { t } = useTranslation("scheduling");
  const hatch = useId().replace(/:/g, "");
  const [active, setActive] = useState<TimelineSummary | null>(null);
  const summary = gpu.summary ?? [];
  if (summary.length === 0 || !gpu.summarized_before) return null;
  const boundaryX = xOf(Date.parse(gpu.summarized_before), span);
  const boundaryText = formatTime(gpu.summarized_before);
  const activeBlock = active ? blockOf(active, span) : null;
  // The key lists each dominant deployment once, in the order they first lead.
  const key = new Map<number, string>();
  for (const slice of summary) if (slice.dominant_deployment_id && !key.has(slice.dominant_deployment_id)) key.set(slice.dominant_deployment_id, slice.dominant_deployment ?? "");
  const hasUndominated = summary.some((slice) => !slice.dominant_deployment_id);
  return (
    <div className="timeline-lane timeline-summary-lane">
      <div className="timeline-lane-label">
        <Typography.Text type="secondary">{t("timeline.summary.lane")}</Typography.Text>
        <ul className="timeline-summary-key" aria-label={t("timeline.summary.keyAria")}>
          {[...key].map(([id, name]) => (
            <li key={id}>
              <span className="scheduling-swatch timeline-summary-swatch" style={{ background: deploymentColor(id) }} aria-hidden="true" />
              {name}
            </li>
          ))}
          {hasUndominated && (
            <li>
              <span className="scheduling-swatch timeline-summary-swatch" style={{ background: deploymentColor(undefined) }} aria-hidden="true" />
              {t("timeline.summary.noDominant")}
            </li>
          )}
        </ul>
      </div>
      <div className="timeline-track">
        <svg viewBox={`0 0 ${CHART_WIDTH} ${LANE_HEIGHT}`} preserveAspectRatio="none" role="group" aria-label={t("timeline.summary.laneAria", { time: boundaryText })}>
          <defs>
            <pattern id={hatch} width={6} height={6} patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
              <rect width={2.5} height={6} fill={colors.white} opacity={0.6} />
            </pattern>
          </defs>
          <rect x={0} y={0} width={CHART_WIDTH} height={LANE_HEIGHT} fill={colors.chartGrid} opacity={0.5} />
          {ticks.map((x) => <line key={x} x1={x} x2={x} y1={0} y2={LANE_HEIGHT} stroke={colors.white} strokeWidth={1} vectorEffect="non-scaling-stroke" />)}
          {summary.map((slice) => {
            const block = blockOf(slice, span);
            const label = summaryText(t, slice);
            return (
              <g key={slice.from}>
                <rect
                  className="timeline-block timeline-summary-block"
                  x={block.x}
                  y={2}
                  width={block.width}
                  height={LANE_HEIGHT - 4}
                  fill={deploymentColor(slice.dominant_deployment_id)}
                  tabIndex={0}
                  role="img"
                  aria-label={label}
                  onMouseEnter={() => setActive(slice)}
                  onMouseLeave={() => setActive(null)}
                  onFocus={() => setActive(slice)}
                  onBlur={() => setActive(null)}
                  onKeyDown={(event) => { if (event.key === "Escape") setActive(null); }}
                />
                <rect x={block.x} y={2} width={block.width} height={LANE_HEIGHT - 4} fill={`url(#${hatch})`} pointerEvents="none" aria-hidden="true" />
              </g>
            );
          })}
          <g role="img" aria-label={t("timeline.summary.boundaryAria", { time: boundaryText })}>
            <BoundaryLine x={boundaryX} />
          </g>
        </svg>
        {active && activeBlock && (
          <div role="tooltip" className="timeline-tooltip" style={{ left: `${((activeBlock.x + activeBlock.width / 2) / CHART_WIDTH) * 100}%` }}>
            {summaryText(t, active)}
          </div>
        )}
      </div>
    </div>
  );
}

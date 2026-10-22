import { useCallback } from "react";
import { Alert, Button, Tooltip, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { api, usePolling, type MonitorRange } from "../../api";
import type {
  MonitorLatency,
  MonitorLatencySegment,
} from "../../monitorRequestsApi";
import { colors } from "../../theme/tokens";
import InfoTitle from "./InfoTitle";
import { SEGMENT_STYLE, segmentLabel } from "./RequestRow";
import { formatMS, type SegmentKind } from "./requestShape";

const POLL_MS = 15_000;

// speedOf is the mean speed quoted for a segment: prefill over the uncached
// prompt, thinking over the reasoning tokens, answering over the rest of the
// output (or the whole decode when the answer could not be told apart). In
// a PD deployment the prompt is computed in the prefill stage; the decode
// instance's prefill only recomputes the last token.
function speedOf(
  segment: MonitorLatencySegment,
  data: MonitorLatency,
  pd: boolean,
): number | null {
  switch (segment.kind) {
    case "pd_prefill":
      return pd ? data.speeds.prefill_tok_s : null;
    case "prefill":
      return pd ? null : data.speeds.prefill_tok_s;
    case "thinking":
      return data.speeds.thinking_tok_s;
    case "answering":
      return data.speeds.answering_tok_s ?? data.speeds.decode_tok_s;
    default:
      return null;
  }
}

// RECOMPUTE_MIN_SHARE is the share of the end-to-end mean a PD deployment's
// first-token recompute has to exceed to be drawn.
const RECOMPUTE_MIN_SHARE = 0.01;

// phaseLabel names a segment in the breakdown. The PD prefill stage is the
// deployment's prefill, so it reads as plain Prefill here (the request rows
// keep the PD name to tell it from the decode instance's recompute).
function phaseLabel(kind: SegmentKind, pd: boolean, t: TFunction): string {
  if (pd && kind === "pd_prefill") return t("requests.segment.prefill");
  return segmentLabel(kind, pd, t);
}

// visibleSegments drops a PD deployment's first-token recompute while it is
// no more than 1% of the end-to-end mean: the decode instance only recomputes
// the last prompt token, so the sliver says nothing and just crowds the legend.
// Elsewhere a prefill segment is the real prefill and always stays. The bar
// is flex-grow proportional, so the rest of it simply fills the width.
function visibleSegments(
  data: MonitorLatency,
  e2eMeanMS: number,
  pd: boolean,
): MonitorLatencySegment[] {
  if (!pd) return data.segments;
  return data.segments.filter(
    (s) => s.kind !== "prefill" || s.mean_ms / e2eMeanMS > RECOMPUTE_MIN_SHARE,
  );
}

// LatencyBreakdown splits the deployment's mean end-to-end time into the
// phases its whole replies went through: one bar whose segments are the
// phases' means, so their lengths add up to the end-to-end mean. Hover or
// focus a segment for its p50 / p95 and mean speed.
export default function LatencyBreakdown({
  deploymentId,
  range,
  pd = false,
}: {
  deploymentId: number;
  range: MonitorRange;
  // pd names the decode instance's queue and prefill as PD has them.
  pd?: boolean;
}) {
  const { t } = useTranslation("monitoring");
  const { t: tCommon } = useTranslation("common");
  const fetcher = useCallback(
    () => api.monitorLatency(deploymentId, range),
    [deploymentId, range],
  );
  const { data, error, loading, refetch } = usePolling<MonitorLatency>(
    fetcher,
    POLL_MS,
    [deploymentId, range],
  );
  const e2e = data?.end_to_end;
  const segments = data && e2e ? visibleSegments(data, e2e.mean_ms, pd) : [];

  return (
    <div className="monitor-latency" aria-busy={loading && !data}>
      <InfoTitle title={t("latency.title")} help={t("latency.help")} />
      {error && (
        <Alert
          type="error"
          showIcon
          style={{ marginTop: 8 }}
          message={t("latency.loadFailed")}
          description={error.message}
          action={
            <Button size="small" onClick={() => refetch()}>
              {tCommon("actions.retry")}
            </Button>
          }
        />
      )}
      {!error && data && !e2e && (
        <Typography.Text
          type="secondary"
          style={{ display: "block", marginTop: 12 }}
        >
          {t("latency.empty")}
        </Typography.Text>
      )}
      {e2e && data && (
        <>
          <div className="monitor-latency-total">
            <span className="monitor-latency-mean">
              {formatMS(e2e.mean_ms)}
            </span>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {t("latency.total", {
                p50: formatMS(e2e.p50_ms),
                p95: formatMS(e2e.p95_ms),
                count: data.requests,
              })}
            </Typography.Text>
          </div>
          <div
            className="monitor-latency-bar"
            role="list"
            aria-label={t("latency.bar")}
          >
            {segments.map((segment) => {
              const share = segment.mean_ms / e2e.mean_ms;
              const speed = speedOf(segment, data, pd);
              const label = phaseLabel(segment.kind, pd, t);
              const detail = (
                <div
                  style={{
                    display: "flex",
                    flexDirection: "column",
                    gap: 2,
                    fontVariantNumeric: "tabular-nums",
                  }}
                >
                  <strong>{label}</strong>
                  <span>
                    {t("latency.mean", {
                      time: formatMS(segment.mean_ms),
                      share: Math.round(share * 100),
                    })}
                  </span>
                  <span>
                    {t("latency.percentiles", {
                      p50: formatMS(segment.p50_ms),
                      p95: formatMS(segment.p95_ms),
                      count: segment.requests,
                    })}
                  </span>
                  {speed != null && (
                    <span>
                      {t("latency.speed", { speed: Math.round(speed) })}
                    </span>
                  )}
                </div>
              );
              return (
                <Tooltip
                  key={segment.kind}
                  title={detail}
                  trigger={["hover", "focus"]}
                  mouseEnterDelay={0.1}
                >
                  <span
                    role="listitem"
                    tabIndex={0}
                    data-kind={segment.kind}
                    aria-label={`${label} ${formatMS(segment.mean_ms)}`}
                    style={{
                      flexGrow: segment.mean_ms,
                      background: SEGMENT_STYLE[segment.kind],
                    }}
                  />
                </Tooltip>
              );
            })}
          </div>
          <div className="monitor-latency-legend">
            {segments.map((segment) => (
              <span key={segment.kind}>
                <i style={{ background: SEGMENT_STYLE[segment.kind] }} />
                <span style={{ color: colors.muted }}>
                  {phaseLabel(segment.kind, pd, t)}
                </span>
                <strong>{formatMS(segment.mean_ms)}</strong>
              </span>
            ))}
          </div>
        </>
      )}
    </div>
  );
}

import type { ReactNode } from "react";
import { Alert, Button, Skeleton, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { MonitorDistributions } from "../../monitorRequestsApi";
import DistributionHeatmap from "./DistributionHeatmap";
import { coverageShare, formatMS, formatTokens, tokenBucketLabel, ttftBucketLabel } from "./requestShape";

// InputOutputHeatmap: how many requests had each prompt and completion size.
export function InputOutputHeatmap({ dist, height }: { dist: MonitorDistributions; height: number }) {
  const { t } = useTranslation("monitoring");
  const bounds = dist.token_buckets;
  return (
    <div>
      <DistributionHeatmap
        cells={dist.input_output}
        height={height - COVERAGE_HEIGHT}
        label={t("distribution.inputOutput")}
        xTitle={t("distribution.inputTokens")}
        yTitle={t("distribution.outputTokens")}
        xTick={(i) => formatTokens(bounds[i] ?? 0)}
        yTick={(i) => formatTokens(bounds[i] ?? 0)}
        xRange={(i) => tokenBucketLabel(bounds, i)}
        yRange={(i) => tokenBucketLabel(bounds, i)}
        readout={(x, y, n, share) => t("distribution.ioReadout", { input: x, output: y, count: n, share })}
        hint={t("distribution.hint")}
        empty={t("distribution.empty")}
      />
      <Coverage dist={dist} field="with_usage" />
    </div>
  );
}

// UncachedTTFTHeatmap: how long the first token took for each amount of
// prompt the cache did not cover - the work prefill had to do.
export function UncachedTTFTHeatmap({ dist, height }: { dist: MonitorDistributions; height: number }) {
  const { t } = useTranslation("monitoring");
  const bounds = dist.token_buckets;
  const ttft = dist.ttft_bounds_ms;
  return (
    <div>
      <DistributionHeatmap
        cells={dist.uncached_ttft}
        height={height - COVERAGE_HEIGHT}
        label={t("distribution.uncachedTTFT")}
        xTitle={t("distribution.uncachedTokens")}
        yTitle={t("distribution.ttft")}
        xTick={(i) => formatTokens(bounds[i] ?? 0)}
        yTick={(i) => (i === 0 ? "0" : formatMS(ttft[i - 1] ?? 0))}
        xRange={(i) => tokenBucketLabel(bounds, i)}
        yRange={(i) => ttftBucketLabel(ttft, i)}
        readout={(x, y, n, share) => t("distribution.utReadout", { uncached: x, ttft: y, count: n, share })}
        hint={t("distribution.hint")}
        empty={t("distribution.emptyCache")}
      />
      <Coverage dist={dist} field="with_cache" />
    </div>
  );
}

// Coverage says how much of the traffic a grid stands for: only requests
// whose reply carried usage (and, for the cache grid, a cache figure on a
// stream) can be placed.
// It keeps its line when empty, so every card is the same height.
function Coverage({ dist, field }: { dist: MonitorDistributions; field: "with_usage" | "with_cache" }) {
  const { t } = useTranslation("monitoring");
  return (
    <Typography.Text type="secondary" style={{ fontSize: 12, display: "block", height: COVERAGE_HEIGHT, lineHeight: `${COVERAGE_HEIGHT}px` }} ellipsis>
      {dist.coverage.total > 0
        ? t(field === "with_usage" ? "distribution.coverageUsage" : "distribution.coverageCache", {
            share: coverageShare(dist, field),
            total: dist.coverage.total,
          })
        : ""}
    </Typography.Text>
  );
}

const COVERAGE_HEIGHT = 20;

// DistributionState renders a distribution card's body through its loading
// and error states at the card's full height.
export function DistributionState({
  state,
  height,
  children,
}: {
  state: { data: MonitorDistributions | null; error: Error | null; loading: boolean; refetch: () => unknown };
  height: number;
  children: (dist: MonitorDistributions) => ReactNode;
}) {
  const { t } = useTranslation("monitoring");
  const { t: tCommon } = useTranslation("common");
  if (state.data) return <>{children(state.data)}</>;
  if (state.error) {
    return (
      <div style={{ height, display: "flex", alignItems: "center" }}>
        <Alert
          type="error"
          showIcon
          style={{ width: "100%" }}
          message={t("distribution.loadFailed")}
          description={state.error.message}
          action={<Button size="small" onClick={() => state.refetch()}>{tCommon("actions.retry")}</Button>}
        />
      </div>
    );
  }
  return (
    <div style={{ height }}>
      <Skeleton active paragraph={{ rows: 4 }} title={false} />
    </div>
  );
}

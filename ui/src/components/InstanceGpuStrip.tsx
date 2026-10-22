import { Alert, Progress, Typography } from "antd";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type MetricSeries } from "../api";
import { colors } from "../theme/tokens";

// Per-GPU telemetry for the GPUs an instance actually holds. This mirrors
// useGpuTelemetry's fetching approach (a handful of node-scoped queries,
// grouped client-side by the "gpu" label) rather than one query per GPU per
// metric, but narrows the rendered set to this instance's gpu_indexes instead
// of the whole node.

// Only the latest point of each series is ever read (see mergeLatest below),
// so the query window only needs to be wide enough to guarantee a recent
// sample exists - a full 5 minutes was needlessly wide for that.
const RANGE = "1m";
const STEP = "10s";
const GIB = 1024 ** 3;

const METRICS = {
  utilization: "gpuworks_gpu_utilization",
  memoryUsed: "gpuworks_gpu_memory_used_bytes",
  memoryTotal: "gpuworks_gpu_memory_total_bytes",
  temperature: "gpuworks_gpu_temperature_celsius",
  power: "gpuworks_gpu_power_watts",
} as const;

// A dGPU instance's own share of each card, as the HAMi interception counts
// it. The card-wide metrics above include every other slice on the card, so
// for such an instance they are context, not its own load.
const SLICE_METRICS = {
  sliceMemoryUsed: "gpuworks_dgpu_memory_used_bytes",
  sliceSM: "gpuworks_dgpu_sm_util",
} as const;

type MetricKey = keyof typeof METRICS | keyof typeof SLICE_METRICS;
type Snapshot = Partial<Record<MetricKey, number>>;

export default function InstanceGpuStrip({ nodeUuid, gpuIndexes, instanceUuid, vramLimitBytes }: {
  nodeUuid: string;
  gpuIndexes: number[];
  // For a dGPU instance: whose slice to read, and the limit it runs under.
  instanceUuid?: string;
  vramLimitBytes?: number;
}) {
  const sliced = !!instanceUuid && (vramLimitBytes ?? 0) > 0;
  const { t } = useTranslation("deployments");
  const [data, setData] = useState<Record<number, Snapshot>>({});
  const [loading, setLoading] = useState(true);
  const indexKey = useMemo(() => gpuIndexes.slice().sort((a, b) => a - b).join(","), [gpuIndexes]);

  useEffect(() => {
    if (!nodeUuid || gpuIndexes.length === 0) {
      setData({});
      setLoading(false);
      return;
    }
    let alive = true;
    const allowed = new Set(gpuIndexes);
    const load = async () => {
      setLoading(true);
      try {
        const queries: Array<{ key: MetricKey; metric: string; instance?: string }> = [
          ...(Object.keys(METRICS) as Array<keyof typeof METRICS>).map((key) => ({ key, metric: METRICS[key] })),
          ...(sliced ? (Object.keys(SLICE_METRICS) as Array<keyof typeof SLICE_METRICS>).map((key) => ({ key, metric: SLICE_METRICS[key], instance: instanceUuid })) : []),
        ];
        const entries = await Promise.all(
          queries.map(async ({ key, metric, instance }) => ({
            key,
            series: (await api.metricsQuery(metric, { node: nodeUuid, ...(instance ? { instance } : {}), range: RANGE, step: STEP })).items,
          })),
        );
        if (!alive) return;
        const next: Record<number, Snapshot> = {};
        for (const idx of gpuIndexes) next[idx] = {};
        for (const { key, series } of entries) mergeLatest(next, key, series, allowed);
        setData(next);
      } catch {
        if (alive) setData({});
      } finally {
        if (alive) setLoading(false);
      }
    };
    void load();
    const timer = setInterval(load, 10000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
    // indexKey is content-derived from gpuIndexes (sorted, joined), so it
    // already changes whenever the set of GPUs actually changes. Including
    // the raw array too would restart the whole fetch fan-out on every parent
    // re-render that passes a new-but-equal array, which is exactly the
    // needless-refetch bug this comment is here to prevent regressing.
  }, [nodeUuid, indexKey, sliced, instanceUuid]);

  const hasAnyMetric = Object.values(data).some((s) =>
    s.utilization != null || s.memoryUsed != null || s.temperature != null || s.power != null || s.sliceMemoryUsed != null,
  );

  if (!hasAnyMetric && !loading) {
    return (
      <Alert
        type="info"
        showIcon
        message={t("instanceGpu.waiting")}
        description={t("instanceGpu.waitingDescription")}
      />
    );
  }

  const utilValues = gpuIndexes.map((idx) => data[idx]?.utilization).filter((v): v is number => v != null);
  const skew = gpuIndexes.length >= 2 ? computeSkew(utilValues) : undefined;

  return (
    <div>
      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(200px, 1fr))", gap: 10 }}>
        {gpuIndexes.map((index) => (
          <GpuTile key={index} index={index} snapshot={data[index] ?? {}} sliceLimit={sliced ? vramLimitBytes : undefined} t={t} />
        ))}
      </div>
      {gpuIndexes.length >= 2 && (
        <Typography.Text type="secondary" style={{ display: "block", marginTop: 8, fontSize: 12 }}>
          {t("instanceGpu.groupSkew", { ratio: skew != null ? `${skew.toFixed(2)}x` : "—" })}
        </Typography.Text>
      )}
    </div>
  );
}

// computeSkew is the deterministic max/min ratio across GPUs currently
// reporting utilization. A zero minimum has no meaningful ratio (division by
// zero), so that case reports "not computable" rather than Infinity.
function computeSkew(values: number[]): number | undefined {
  if (values.length < 2) return undefined;
  const max = Math.max(...values);
  const min = Math.min(...values);
  if (min <= 0) return undefined;
  return max / min;
}

function mergeLatest(target: Record<number, Snapshot>, key: MetricKey, series: MetricSeries[], allowed: Set<number>) {
  for (const item of series) {
    const gpu = Number(item.labels?.gpu);
    if (!Number.isFinite(gpu) || !allowed.has(gpu)) continue;
    const point = item.points?.[item.points.length - 1];
    if (!point) continue;
    target[gpu] = { ...(target[gpu] ?? {}), [key]: point[1] };
  }
}

function GpuTile({ index, snapshot, sliceLimit, t }: { index: number; snapshot: Snapshot; sliceLimit?: number; t: (key: string, options?: Record<string, unknown>) => string }) {
  const utilPct = snapshot.utilization != null ? Math.max(0, Math.min(100, snapshot.utilization)) : undefined;
  const memPct =
    snapshot.memoryUsed != null && snapshot.memoryTotal
      ? Math.min(100, (snapshot.memoryUsed / snapshot.memoryTotal) * 100)
      : undefined;
  if (sliceLimit) {
    const slicePct = snapshot.sliceMemoryUsed != null ? Math.min(100, (snapshot.sliceMemoryUsed / sliceLimit) * 100) : undefined;
    const smPct = snapshot.sliceSM != null ? Math.max(0, Math.min(100, snapshot.sliceSM)) : undefined;
    return (
      <div style={{ border: `1px solid ${colors.borderSoft}`, borderRadius: 4, padding: "10px 12px", background: colors.white }}>
        <Typography.Text strong style={{ display: "block", marginBottom: 6 }}>{t("instanceGpu.sliceTitle", { index, limit: (sliceLimit / GIB).toFixed(1) })}</Typography.Text>
        <TileBar
          label={t("instanceGpu.sliceVram")}
          percent={slicePct}
          value={snapshot.sliceMemoryUsed == null ? "—" : `${(snapshot.sliceMemoryUsed / GIB).toFixed(1)} / ${(sliceLimit / GIB).toFixed(1)} GiB`}
        />
        <TileBar label={t("instanceGpu.sliceSM")} percent={smPct} value={smPct == null ? "—" : `${smPct.toFixed(0)}%`} />
        <Typography.Text type="secondary" style={{ display: "block", marginTop: 6, fontSize: 12 }}>
          {t("instanceGpu.wholeCard", {
            util: utilPct == null ? "—" : `${utilPct.toFixed(0)}%`,
            vram: snapshot.memoryUsed == null ? "—" : `${(snapshot.memoryUsed / GIB).toFixed(1)} / ${snapshot.memoryTotal ? (snapshot.memoryTotal / GIB).toFixed(0) : "—"} GiB`,
          })}
        </Typography.Text>
      </div>
    );
  }
  return (
    <div style={{ border: `1px solid ${colors.borderSoft}`, borderRadius: 4, padding: "10px 12px", background: colors.white }}>
      <Typography.Text strong style={{ display: "block", marginBottom: 6 }}>GPU{index}</Typography.Text>
      <TileBar label={t("instanceGpu.utilization")} percent={utilPct} value={utilPct == null ? "—" : `${utilPct.toFixed(0)}%`} />
      <TileBar
        label={t("instanceGpu.vram")}
        percent={memPct}
        // A used figure with no known total has no percentage to show - a bar
        // pinned at 0% would read as "empty" rather than "unknown", so it is
        // hidden instead of drawn wrong.
        hideBar={snapshot.memoryTotal == null}
        value={
          snapshot.memoryUsed == null
            ? "—"
            : `${(snapshot.memoryUsed / GIB).toFixed(1)} / ${snapshot.memoryTotal ? (snapshot.memoryTotal / GIB).toFixed(0) : "—"} GiB`
        }
      />
      <div style={{ display: "flex", justifyContent: "space-between", marginTop: 6 }}>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {t("instanceGpu.temperature")} {snapshot.temperature == null ? "—" : `${snapshot.temperature.toFixed(0)}C`}
        </Typography.Text>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {t("instanceGpu.power")} {snapshot.power == null ? "—" : `${snapshot.power.toFixed(0)}W`}
        </Typography.Text>
      </div>
    </div>
  );
}

function TileBar({ label, percent, value, hideBar }: { label: string; percent?: number; value: string; hideBar?: boolean }) {
  return (
    <div style={{ marginBottom: 6 }}>
      <div style={{ display: "flex", justifyContent: "space-between", marginBottom: 2 }}>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>{label}</Typography.Text>
        <Typography.Text style={{ fontSize: 12 }}>{value}</Typography.Text>
      </div>
      {!hideBar && (
        <Progress percent={percent ?? 0} size="small" showInfo={false} strokeColor={colors.chartPrimary} trailColor={colors.borderSoft} />
      )}
    </div>
  );
}

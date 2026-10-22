import { useEffect, useMemo, useState } from "react";
import { api, type GPU, type MetricSeries } from "../api";
import { cardKey } from "./cardIdentity";

// GpuSnapshot is one card's latest reading of the metrics the agent reports
// every 10 s; a field the agent has not reported yet is absent.
export type GpuSnapshot = {
  utilization?: number;
  memoryUsedBytes?: number;
  temperatureC?: number;
  powerWatts?: number;
  updatedAtMS?: number;
};

type MetricKey = "utilization" | "memoryUsedBytes" | "temperatureC" | "powerWatts";

const METRICS: Record<MetricKey, string> = {
  utilization: "gpuworks_gpu_utilization",
  memoryUsedBytes: "gpuworks_gpu_memory_used_bytes",
  temperatureC: "gpuworks_gpu_temperature_celsius",
  powerWatts: "gpuworks_gpu_power_watts",
};

// useGpuTelemetry reads a node's GPU metrics with one node-scoped query per
// metric and refreshes them every 10 s. Readings are keyed by the vendor
// CLI's device number; a card without one has none to look up.
export function useGpuTelemetry(nodeUuid: string, gpus: GPU[]) {
  const [data, setData] = useState<Record<number, GpuSnapshot>>({});
  const [loading, setLoading] = useState(false);
  const gpuKey = useMemo(() => gpus.map(cardKey).join(","), [gpus]);

  useEffect(() => {
    if (!nodeUuid || gpus.length === 0) {
      setData({});
      return;
    }
    let alive = true;
    const load = async () => {
      setLoading(true);
      try {
        const entries = await Promise.all(
          (Object.keys(METRICS) as MetricKey[]).map(async (key) => ({
            key,
            series: (await api.metricsQuery(METRICS[key], {
              node: nodeUuid,
              range: "5m",
              step: "10s",
            })).items,
          })),
        );
        if (!alive) return;
        const next: Record<number, GpuSnapshot> = {};
        for (const gpu of gpus) if (gpu.index >= 0) next[gpu.index] = {};
        for (const { key, series } of entries) mergeLatest(next, key, series);
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
  }, [nodeUuid, gpuKey, gpus]);

  const hasAnyMetric = Object.values(data).some((s) =>
    s.utilization != null ||
    s.memoryUsedBytes != null ||
    s.temperatureC != null ||
    s.powerWatts != null,
  );
  const snapshotOf = (gpu: GPU): GpuSnapshot =>
    (gpu.index >= 0 ? data[gpu.index] : undefined) ?? {};
  return { snapshotOf, loading, hasAnyMetric };
}

function mergeLatest(target: Record<number, GpuSnapshot>, key: MetricKey, series: MetricSeries[]) {
  for (const item of series) {
    const gpu = Number(item.labels?.gpu);
    if (!Number.isFinite(gpu)) continue;
    const point = item.points?.[item.points.length - 1];
    if (!point) continue;
    const existing = target[gpu] ?? {};
    target[gpu] = {
      ...existing,
      [key]: point[1],
      updatedAtMS: Math.max(existing.updatedAtMS ?? 0, point[0]),
    };
  }
}

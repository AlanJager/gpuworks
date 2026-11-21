// --- Metrics & logs --------------------------------------------------------
// MetricPoint is one sample as [unix ms, value]; GET /metrics/query and
// GET /monitor/series both answer series of this shape.
export type MetricPoint = [number, number];
export interface MetricSeries {
  labels: Record<string, string>;
  points: MetricPoint[];
}

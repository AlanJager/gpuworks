import { getJSON, type List } from "../http";
import type { MetricSeries } from "../metrics";
import type { LogsBucket, LogsFilter, LogsPage, LogsWindow } from "../logs";

export const logsApi = {
  metricsQuery: (
    metric: string,
    params: {
      range?: string;
      start?: number;
      end?: number;
      step?: string;
      node?: string;
      gpu?: string;
      instance?: string;
      benchmark?: string;
      point?: string;
      iface?: string;
      hca?: string;
      role?: string;
    } = {},
  ) => {
    const q = new URLSearchParams({ metric });
    if (params.range) q.set("range", params.range);
    if (params.step) q.set("step", params.step);
    if (params.node) q.set("node", params.node);
    if (params.gpu) q.set("gpu", params.gpu);
    if (params.instance) q.set("instance", params.instance);
    if (params.start) q.set("start", String(params.start));
    if (params.end) q.set("end", String(params.end));
    if (params.benchmark) q.set("benchmark", params.benchmark);
    if (params.point) q.set("point", params.point);
    if (params.iface) q.set("iface", params.iface);
    if (params.hca) q.set("hca", params.hca);
    if (params.role) q.set("role", params.role);
    return getJSON<List<MetricSeries>>(`/api/v1/metrics/query?${q.toString()}`);
  },
  logsSearch: (f: LogsFilter = {}) => {
    const p = new URLSearchParams();
    if (f.q) p.set("q", f.q);
    if (f.instance) p.set("instance", f.instance);
    if (f.instances && f.instances.length)
      p.set("instances", f.instances.join(","));
    if (f.node) p.set("node", f.node);
    if (f.level) p.set("level", f.level);
    if (f.source !== undefined) p.set("source", f.source);
    if (f.from) p.set("from", String(f.from));
    if (f.to) p.set("to", String(f.to));
    if (f.cursor_ts) p.set("cursor_ts", String(f.cursor_ts));
    if (f.cursor_id) p.set("cursor_id", String(f.cursor_id));
    if (f.limit) p.set("limit", String(f.limit));
    if (f.with_total) p.set("with_total", "1");
    return getJSON<LogsPage>(`/api/v1/logs/search?${p.toString()}`);
  },
  logsContext: (
    instance: string,
    node: string,
    ts: number,
    id: number,
    before: number,
    after: number,
  ) =>
    getJSON<LogsWindow>(
      `/api/v1/logs/context?instance=${encodeURIComponent(instance)}&node=${encodeURIComponent(node)}&ts=${ts}&id=${id}&before=${before}&after=${after}`,
    ),
  logsHistogram: (f: LogsFilter = {}, bucketMS = 60_000) => {
    const p = new URLSearchParams();
    if (f.q) p.set("q", f.q);
    if (f.instance) p.set("instance", f.instance);
    if (f.instances && f.instances.length)
      p.set("instances", f.instances.join(","));
    if (f.node) p.set("node", f.node);
    if (f.level) p.set("level", f.level);
    if (f.source !== undefined) p.set("source", f.source);
    if (f.from) p.set("from", String(f.from));
    if (f.to) p.set("to", String(f.to));
    p.set("bucket", String(bucketMS));
    return getJSON<List<LogsBucket>>(`/api/v1/logs/histogram?${p.toString()}`);
  },
};

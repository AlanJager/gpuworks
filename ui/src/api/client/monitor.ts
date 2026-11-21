import type { MonitorLatency, MonitorRequestLookup, MonitorRequestLookupQuery, MonitorDistributions, MonitorRequests, MonitorRequestsQuery } from "../../monitorRequestsApi";
import { monitorDistributionsURL, monitorLatencyURL, monitorRequestsURL, monitorRequestLookupURL } from "../../monitorRequestsApi";
import type { MonitorRange } from "../../monitorRange";
import { rangeParams } from "../../monitorRange";
import { getJSON, type List } from "../http";
import { monitorSeriesURL, type MonitorDeployment, type MonitorDeploymentSignals, type MonitorMetricSeriesInfo, type MonitorOverview, type MonitorSeries, type MonitorSeriesParams } from "../monitor";

export const monitorApi = {
  monitorOverview: (range: MonitorRange) =>
    getJSON<MonitorOverview>(
      `/api/v1/monitor/overview?${new URLSearchParams(rangeParams(range))}`,
    ),
  monitorDeployment: (id: number, range: MonitorRange) =>
    getJSON<MonitorDeployment>(
      `/api/v1/monitor/deployments/${id}?${new URLSearchParams(rangeParams(range))}`,
    ),
  monitorDeploymentSignals: (id: number, window: { start: number; end: number; step: string }) =>
    getJSON<MonitorDeploymentSignals>(
      `/api/v1/monitor/deployments/${id}/signals?${new URLSearchParams({ start: String(window.start), end: String(window.end), step: window.step })}`,
    ),
  monitorRequests: (id: number, query: MonitorRequestsQuery, signal?: AbortSignal) =>
    getJSON<MonitorRequests>(monitorRequestsURL(id, query), signal),
  monitorRequestLookup: (query: MonitorRequestLookupQuery, signal?: AbortSignal) =>
    getJSON<MonitorRequestLookup>(monitorRequestLookupURL(query), signal),
  monitorLatency: (id: number, range: MonitorRange) => getJSON<MonitorLatency>(monitorLatencyURL(id, range)),
  monitorDistributions: (range: MonitorRange, deploymentId?: number) =>
    getJSON<MonitorDistributions>(monitorDistributionsURL(range, deploymentId)),
  monitorMetrics: () => getJSON<List<MonitorMetricSeriesInfo>>("/api/v1/monitor/metrics"),
  monitorSeries: (params: MonitorSeriesParams) =>
    getJSON<List<MonitorSeries>>(monitorSeriesURL(params)),
};

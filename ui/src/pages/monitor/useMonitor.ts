import { useCallback } from "react";
import { api, useEvents, type MonitorDeployment, type MonitorOverview, type MonitorRange } from "../../api";
import type { MonitorDistributions } from "../../monitorRequestsApi";
import { useMonitorPolling } from "./useMonitorPolling";
import { useTimeRange } from "./TimeRangeContext";

// Both hooks poll every 10s while "follow latest" is on (interval 0 disables
// the timer entirely, per the monitor polling contract) and additionally refetch the
// instant a relevant SSE event arrives, so a state change shows up without
// waiting for the next tick.
const POLL_MS = 10000;

export function useMonitorOverview(range: MonitorRange) {
  const { follow } = useTimeRange();
  const fetcher = useCallback(() => api.monitorOverview(range), [range]);
  const { data, error, loading, updatedAt, refetch, refresh } = useMonitorPolling<MonitorOverview>(
    fetcher,
    follow ? POLL_MS : 0,
    range,
  );
  useEvents("deployment", refresh);
  useEvents("instance", refresh);
  return { data, error, loading, updatedAt, refetch };
}

// An id that is not a positive integer (a malformed route param, e.g.
// "/monitor/d/not-a-number") never reaches the network: the caller is
// expected to render its own "not found" state instead, and a request for
// deployment "-1" would be a wasted round trip to a 400/404 the UI never
// shows.
function isValidDeploymentId(id: number): boolean {
  return Number.isInteger(id) && id > 0;
}

export function useMonitorDeployment(id: number, range: MonitorRange) {
  const { follow } = useTimeRange();
  const skip = !isValidDeploymentId(id);
  const fetcher = useCallback(
    () => (skip ? Promise.resolve(null as unknown as MonitorDeployment) : api.monitorDeployment(id, range)),
    [id, range, skip],
  );
  const { data, error, loading, updatedAt, refetch, refresh } = useMonitorPolling<MonitorDeployment>(
    fetcher,
    !skip && follow ? POLL_MS : 0,
    `${id}:${range}`,
  );
  useEvents("deployment", refresh);
  useEvents("instance", refresh);
  return { data, error, loading, updatedAt, refetch };
}

// Distributions move in five-minute buckets; a 30s poll keeps the current
// bucket fresh without re-summing the range every 10s. deploymentId 0 is the
// whole cluster.
const DISTRIBUTIONS_POLL_MS = 30000;

export function useMonitorDistributions(range: MonitorRange, deploymentId = 0) {
  const { follow } = useTimeRange();
  const skip = deploymentId !== 0 && !isValidDeploymentId(deploymentId);
  const fetcher = useCallback(
    () => (skip ? Promise.resolve(null as unknown as MonitorDistributions) : api.monitorDistributions(range, deploymentId || undefined)),
    [range, deploymentId, skip],
  );
  return useMonitorPolling<MonitorDistributions>(fetcher, !skip && follow ? DISTRIBUTIONS_POLL_MS : 0, `${deploymentId}:${range}`);
}

import { getJSON, listQueryString, sendJSON, type CursorPage, type ListPage, type ListQuery } from "../http";
import type { WakeOptions, WakeResult } from "../wake";
import type { Instance, InstanceEvent, InstanceLaunchFacts } from "../instances";
import type { Deployment } from "../deployments";
import { eventPageQuery } from "./transport";
import { wakeOnRequestQuery, wakeQuery } from "./deployments";

export const instancesApi = {
  /** One page of GET /instances, newest first, searched on the server. */
  instancesPage: (query: ListQuery) => getJSON<ListPage<Instance>>(`/api/v1/instances?${listQueryString(query)}`),
  instanceLaunchFacts: (uuid: string) =>
    getJSON<InstanceLaunchFacts>(
      `/api/v1/instances/${encodeURIComponent(uuid)}/launch-facts`,
    ),
  // sleepInstance and wakeInstance order the single-node replica an instance
  // leads; the deployment's other replicas keep serving.
  sleepInstance: (uuid: string, level: number, mode: "wait" | "abort" = "wait", wakeOnRequest?: boolean) =>
    sendJSON<void>("POST", `/api/v1/instances/${encodeURIComponent(uuid)}/sleep?level=${level}&mode=${mode}${wakeOnRequestQuery(wakeOnRequest)}`),
  wakeInstance: (uuid: string, opts: WakeOptions = {}) =>
    sendJSON<WakeResult | void>("POST", `/api/v1/instances/${encodeURIComponent(uuid)}/wake${wakeQuery(opts)}`),
  // restartInstance replaces the replica an instance fills (a follower names
  // its whole generation); drain lets it finish its requests first.
  restartInstance: (uuid: string, drain = true) =>
    sendJSON<Deployment>("POST", `/api/v1/instances/${encodeURIComponent(uuid)}/restart${drain ? "" : "?drain=false"}`),
  // The timeline is newest-first and cursor paginated: `before` is the id of
  // the last entry of the page already held. The server folds repeated
  // milestones, so a page is milestones, never progress samples.
  instanceEvents: (uuid: string, page?: { limit?: number; before?: number }) =>
    getJSON<CursorPage<InstanceEvent>>(
      `/api/v1/instances/${uuid}/events${eventPageQuery(page)}`,
    ),
};

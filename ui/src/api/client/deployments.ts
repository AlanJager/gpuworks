import { getJSON, listQueryString, sendJSON, type CursorPage, type List, type ListQuery } from "../http";
import type { CatalogTemplateExport } from "../inferenceTemplates";
import type { DistributedPreflightResponse } from "../evaluate";
import type { WakeOptions, WakeResult } from "../wake";
import type { InstanceEvent } from "../instances";
import type { Deployment, DeploymentCandidateStatus, DeploymentCandidates, DeploymentDraft, DeploymentEditBody, DeploymentEditPlan, DeploymentEditResult, DeploymentPage, NodeReplacement, PdScaleRequest, PdScaleUnitInput, UnitSuggestion, UnitsPlacementMode } from "../deployments";
import type { DGPUQuotaResult } from "../dgpus";
import { eventPageQuery } from "./transport";

// wakeOnRequestQuery is a sleep order's wake_on_request parameter; none
// leaves the choice to the server's default.
export function wakeOnRequestQuery(wakeOnRequest?: boolean): string {
  return wakeOnRequest === undefined ? "" : `&wake_on_request=${wakeOnRequest}`;
}

export function wakeQuery(opts: WakeOptions): string {
  const query = new URLSearchParams();
  if (opts.role) query.set("role", opts.role);
  if (opts.partial) query.set("partial", "true");
  if (opts.wait) query.set("wait", "true");
  if (opts.yield) {
    query.set("yield", "true");
    if (opts.yieldLevel) query.set("yield_level", String(opts.yieldLevel));
  }
  const qs = query.toString();
  return qs ? `?${qs}` : "";
}

// editDeployment saves a draft edit; with dryRun it answers the plan (changes
// with their layers, the ways to apply) and writes nothing.
function editDeployment(id: number, body: DeploymentEditBody, opts: { dryRun: true }): Promise<DeploymentEditPlan>;
function editDeployment(id: number, body: DeploymentEditBody, opts?: { dryRun?: false }): Promise<DeploymentEditResult>;
function editDeployment(id: number, body: DeploymentEditBody, opts: { dryRun?: boolean } = {}) {
  return opts.dryRun
    ? sendJSON<DeploymentEditPlan>("PUT", `/api/v1/deployments/${id}?dry_run=1`, body)
    : sendJSON<DeploymentEditResult>("PUT", `/api/v1/deployments/${id}`, body);
}

export const deploymentsApi = {
  deployments: () => getJSON<List<Deployment>>("/api/v1/deployments"),
  deploymentsPage: (query: ListQuery) => getJSON<DeploymentPage>(`/api/v1/deployments?${listQueryString(query)}`),
  deployment: (id: number) => getJSON<Deployment>(`/api/v1/deployments/${id}`),
  setDeploymentRouting: (id: number, routing_policy: "least_request" | "prefix_affinity" | "failover") =>
    sendJSON<Deployment>("PUT", `/api/v1/deployments/${id}/routing`, { routing_policy }),
  // threshold_sec 0 keeps the current threshold.
  setDeploymentHealthCheck: (id: number, enabled: boolean, threshold_sec = 0) =>
    sendJSON<Deployment>("PUT", `/api/v1/deployments/${id}/health-check`, { enabled, threshold_sec }),
  setDeploymentTokenize: (id: number, enabled: boolean) =>
    sendJSON<Deployment>("PUT", `/api/v1/deployments/${id}/tokenize`, { enabled }),
  deploymentDraft: (id: number) =>
    getJSON<DeploymentDraft>(`/api/v1/deployments/${id}/draft`),
  editDeployment,
  exportCatalogTemplate: (id: number, includeEnv: string[] = [], signal?: AbortSignal) =>
    sendJSON<CatalogTemplateExport>(
      "POST",
      `/api/v1/deployments/${id}/catalog-template`,
      includeEnv.length ? { include_env: includeEnv } : {},
      signal,
    ),
  cloneDeployment: (id: number, name?: string) =>
    sendJSON<Deployment>(
      "POST",
      `/api/v1/deployments/${id}/clone`,
      name ? { name } : {},
    ),
  createDeployment: (body: unknown) =>
    sendJSON<Deployment>("POST", "/api/v1/deployments", body),
  stopDeployment: (id: number) =>
    sendJSON<void>("POST", `/api/v1/deployments/${id}/stop`),
  // startDeployment brings a stopped deployment back from the configuration
  // it kept; each replica starts with its full retry budget.
  startDeployment: (id: number) =>
    sendJSON<Deployment>("POST", `/api/v1/deployments/${id}/start`),
  deleteDeployment: (id: number, force = false) =>
    sendJSON<void>(
      "DELETE",
      `/api/v1/deployments/${id}${force ? "?force=true" : ""}`,
    ),
  // sleepDeployment orders every live instance to sleep, or with role only a
  // PD deployment's instances of that role. wakeOnRequest says whether a
  // request wakes what it puts to sleep; absent, the server's default for the
  // deployment (sleep_wakes_on_request).
  sleepDeployment: (id: number, level: number, mode: "wait" | "abort" = "wait", role?: string, wakeOnRequest?: boolean) =>
    sendJSON<void>("POST", `/api/v1/deployments/${id}/sleep?level=${level}&mode=${mode}${role ? `&role=${encodeURIComponent(role)}` : ""}${wakeOnRequestQuery(wakeOnRequest)}`),
  // releaseSleepHold lets requests wake the deployment's held sleepers
  // without waking them; it answers the instances released.
  releaseSleepHold: (id: number) =>
    sendJSON<{ released: string[] }>("POST", `/api/v1/deployments/${id}/wake-on-request`),
  // wakeDeployment wakes the deployment (or one PD role) all or nothing;
  // partial wakes the sleepers whose cards can take them back and lists the
  // rest; wait leaves the rest waiting for room, and yield first puts the
  // occupants of the needed cards to sleep.
  wakeDeployment: (id: number, opts: WakeOptions = {}) =>
    sendJSON<WakeResult>("POST", `/api/v1/deployments/${id}/wake${wakeQuery(opts)}`),
  // endWakeWaiting cancels the deployment's waiting wakes; it answers with
  // the instances no longer waiting.
  endWakeWaiting: (id: number) =>
    sendJSON<List<string>>("DELETE", `/api/v1/deployments/${id}/wake-waiting`),
  // The wait before a lost node's replica moves.
  setNodeReplacement: (id: number, body: NodeReplacement) =>
    sendJSON<NodeReplacement>("PUT", `/api/v1/deployments/${id}/node-replacement`, body),
  // updateDeploymentPd rescales a PD deployment's role units and/or fallback
  // policy (ADR 0028); mode, TP and kv_load_failure are fixed once instances
  // start, so the request never carries them.
  updateDeploymentPd: (id: number, body: PdScaleRequest) =>
    sendJSON<Deployment>("PUT", `/api/v1/deployments/${id}/pd`, body),
  // setPlacementMode hands a multi-node or PD deployment's node choice to the
  // platform ("auto": a PD unit's pinned cards are dropped) or back to the
  // operator ("manual": the nodes it has stay). Nothing restarts.
  setPlacementMode: (id: number, mode: UnitsPlacementMode) =>
    sendJSON<Deployment>("PUT", `/api/v1/deployments/${id}/placement-mode`, { mode }),
  // restartDeployment replaces every replica of a deployment, whatever its
  // shape: "rolling" (the server's default) one at a time, each draining
  // while its replacement starts; "all" stops every instance at once. Every
  // replica gets its retry budget back.
  restartDeployment: (id: number, strategy: "rolling" | "all" = "rolling") =>
    sendJSON<Deployment>("POST", `/api/v1/deployments/${id}/restart`, { strategy }),
  // retryDeploymentUnit gives one unit its retry budget back (its failed
  // attempts are retired) so the controller rebuilds it; other units are not
  // touched. role is empty outside PD.
  retryDeploymentUnit: (id: number, unit: { role: string; index: number }) =>
    sendJSON<Deployment>("POST", `/api/v1/deployments/${id}/units/retry`, unit),
  // deploymentCandidates pages through the cluster as the deployment's next
  // replica sees it, optionally only nodes of one status.
  deploymentCandidates: (id: number, page: { status?: DeploymentCandidateStatus; offset?: number; limit?: number } = {}) => {
    const query = new URLSearchParams();
    if (page.status) query.set("status", page.status);
    if (page.offset) query.set("offset", String(page.offset));
    if (page.limit) query.set("limit", String(page.limit));
    const suffix = query.toString() ? `?${query}` : "";
    return getJSON<DeploymentCandidates>(`/api/v1/deployments/${id}/candidates${suffix}`);
  },
  // setDeploymentDGPUQuota sets the dGPU memory limit a deployment's replicas
  // run under, at once: running slices are held to it in place where that is
  // safe; the answer names the ones that take it at their next restart.
  setDeploymentDGPUQuota: (id: number, body: { vram_limit_gib: number }) =>
    sendJSON<DGPUQuotaResult>("PUT", `/api/v1/deployments/${id}/dgpu`, body),
  // setDeploymentRequirements replaces where a single-node deployment's
  // replicas may run from now on; running replicas stay where they are. The
  // vendor is kept when unstated and cannot change.
  setDeploymentRequirements: (id: number, body: { node_requirements: Record<string, string[]>; placement_strategy?: string }) =>
    sendJSON<Deployment>("PUT", `/api/v1/deployments/${id}/requirements`, body),
  // suggestUnit proposes the nodes of one more multi-node replica, given the
  // replicas listed so far; nothing is written until the list is saved.
  suggestUnit: (id: number, units: PdScaleUnitInput[]) =>
    sendJSON<UnitSuggestion>("POST", `/api/v1/deployments/${id}/units/suggest`, { units }),
  // scaleReplicas sets an ordinary single-node deployment's replica count
  // (remove names the instances whose replicas go when it shrinks; place_on
  // names nodes the added replicas start on first, at most as many as are
  // added), or lists a multi-node deployment's replicas.
  scaleReplicas: (id: number, body: { replicas: number; remove?: string[]; place_on?: string[] } | { units: PdScaleUnitInput[] }) =>
    sendJSON<Deployment>("PUT", `/api/v1/deployments/${id}/replicas`, body),
  distributedPreflight: (body: {
    node_uuids: string[];
    port?: number;
    master_addr?: string;
    bandwidth?: boolean;
    seconds?: number;
  }) =>
    sendJSON<DistributedPreflightResponse>(
      "POST",
      "/api/v1/deployments/preflight",
      body,
    ),
  deploymentEvents: (id: number, page?: { limit?: number; before?: number }) =>
    getJSON<CursorPage<InstanceEvent>>(
      `/api/v1/deployments/${id}/events${eventPageQuery(page)}`,
    ),
};

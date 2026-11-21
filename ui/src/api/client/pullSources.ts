import { getJSON, sendJSON, type List } from "../http";
import type { Credential, CredentialRequest, MetadataSources, PullProxy, PullProxyRequest, PullSourceRule, PullSourceRuleRequest, PullSourceRuleTestRequest, SourceRuleTestResult, SyncGroup, SyncGroupRequest } from "../pullSources";

export const pullSourcesApi = {
  pullSourceRules: () => getJSON<List<PullSourceRule>>("/api/v1/pull/source-rules"),
  createPullSourceRule: (body: PullSourceRuleRequest) =>
    sendJSON<PullSourceRule>("POST", "/api/v1/pull/source-rules", body),
  updatePullSourceRule: (id: number, body: PullSourceRuleRequest) =>
    sendJSON<PullSourceRule>("PUT", `/api/v1/pull/source-rules/${id}`, body),
  deletePullSourceRule: (id: number) =>
    sendJSON<void>("DELETE", `/api/v1/pull/source-rules/${id}`),
  /** ids lists every rule exactly once, in the order they are tried. */
  orderPullSourceRules: (ids: number[]) =>
    sendJSON<List<PullSourceRule>>("PUT", "/api/v1/pull/source-rules/order", { ids }),
  /** The management node's candidate bases for reading a source's metadata in the browser. */
  metadataSources: (source: "huggingface" | "modelscope", signal?: AbortSignal) =>
    getJSON<MetadataSources>(`/api/v1/pull/metadata-sources?source=${encodeURIComponent(source)}`, signal),
  testPullSourceRule: (body: PullSourceRuleTestRequest) =>
    sendJSON<SourceRuleTestResult>("POST", "/api/v1/pull/source-rules/test", body),
  pullProxies: () => getJSON<List<PullProxy>>("/api/v1/pull/proxies"),
  createPullProxy: (body: PullProxyRequest) =>
    sendJSON<PullProxy>("POST", "/api/v1/pull/proxies", body),
  updatePullProxy: (id: number, body: PullProxyRequest) =>
    sendJSON<PullProxy>("PUT", `/api/v1/pull/proxies/${id}`, body),
  deletePullProxy: (id: number) =>
    sendJSON<void>("DELETE", `/api/v1/pull/proxies/${id}`),
  /** ids lists every proxy entry exactly once, in the new order. */
  orderPullProxies: (ids: number[]) =>
    sendJSON<List<PullProxy>>("PUT", "/api/v1/pull/proxies/order", { ids }),
  syncGroups: () => getJSON<List<SyncGroup>>("/api/v1/pull/sync-groups"),
  createSyncGroup: (body: SyncGroupRequest) =>
    sendJSON<SyncGroup>("POST", "/api/v1/pull/sync-groups", body),
  updateSyncGroup: (id: number, body: SyncGroupRequest) =>
    sendJSON<SyncGroup>("PUT", `/api/v1/pull/sync-groups/${id}`, body),
  deleteSyncGroup: (id: number) =>
    sendJSON<void>("DELETE", `/api/v1/pull/sync-groups/${id}`),
  credentials: () => getJSON<List<Credential>>("/api/v1/pull/credentials"),
  createCredential: (body: CredentialRequest) =>
    sendJSON<Credential>("POST", "/api/v1/pull/credentials", body),
  updateCredential: (id: number, body: CredentialRequest) =>
    sendJSON<Credential>("PUT", `/api/v1/pull/credentials/${id}`, body),
  deleteCredential: (id: number) =>
    sendJSON<void>("DELETE", `/api/v1/pull/credentials/${id}`),
  testCredential: (id: number) =>
    sendJSON<{ ok: boolean; message?: string }>("POST", `/api/v1/pull/credentials/${id}/test`),
};

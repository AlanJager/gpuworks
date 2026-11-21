import { getJSON, sendJSON, type List } from "../http";
import type { ConnectionCheckEvent, ProviderTestOverride, ProviderTestRequest } from "../connectionTest";
import type { CloudProvider, ProviderCluster, ProviderEditConfig, ProviderHPCClusterList, ProviderInstallRequest, ProviderInstallResponse, ProviderInstance, ProviderKindList } from "../providers";
import { streamConnectionTest } from "./transport";

export const providersApi = {
  providerClusters: (id: number) => getJSON<List<ProviderCluster>>(`/api/v1/providers/${id}/clusters`),
  /** fresh waits for a new read of the cloud (after a sync); otherwise the last listing comes at once. */
  providerHPCClusters: (id: number, fresh = false) => getJSON<ProviderHPCClusterList>(`/api/v1/providers/${id}/hpc-clusters${fresh ? "?fresh=1" : ""}`),

  providerKinds: () => getJSON<ProviderKindList>("/api/v1/providers/kinds"),
  providers: () => getJSON<List<CloudProvider>>("/api/v1/providers"),
  provider: (id: number) => getJSON<ProviderEditConfig>(`/api/v1/providers/${id}`),
  addProvider: (body: {
    kind: string;
    name: string;
    endpoint?: string;
    api_key?: string;
    public_key?: string;
    private_key?: string;
    extra?: Record<string, string>;
  }) =>
    sendJSON<{ id: number; kind: string; name: string }>(
      "POST",
      "/api/v1/providers",
      body,
    ),
  updateProvider: (
    id: number,
    body: {
      name: string;
      endpoint?: string;
      api_key?: string;
      public_key?: string;
      private_key?: string;
      extra?: Record<string, string>;
    },
  ) =>
    sendJSON<{ id: number; kind: string; name: string }>(
      "PUT",
      `/api/v1/providers/${id}`,
      body,
    ),
  deleteProvider: (id: number) =>
    sendJSON<void>("DELETE", `/api/v1/providers/${id}`),
  syncProvider: (id: number) =>
    sendJSON<{ count: number; instances: unknown[] }>(
      "POST",
      `/api/v1/providers/${id}/sync`,
    ),
  providerInstances: (id: number) =>
    getJSON<List<ProviderInstance>>(`/api/v1/providers/${id}/instances`),
  powerProviderInstance: (
    id: number,
    externalId: string,
    action: "on" | "off",
  ) =>
    sendJSON<void>(
      `POST`,
      `/api/v1/providers/${id}/instances/${externalId}/power`,
      { action },
    ),
  installAgent: (id: number, body: ProviderInstallRequest) =>
    sendJSON<ProviderInstallResponse>(
      "POST",
      `/api/v1/providers/${id}/install`,
      body,
    ),
  /** Tests an account that is not saved yet, from the values of its form. */
  testProviderConnection: (
    req: ProviderTestRequest,
    onCheck: (c: ConnectionCheckEvent) => void,
    signal?: AbortSignal,
  ) => streamConnectionTest("/api/v1/providers/test", req, onCheck, signal),
  /** Tests a saved account: with no override, its stored credentials as they are; an override's fields replace the stored ones. */
  testSavedProviderConnection: (
    id: number,
    onCheck: (c: ConnectionCheckEvent) => void,
    override?: ProviderTestOverride,
    signal?: AbortSignal,
  ) => streamConnectionTest(`/api/v1/providers/${id}/test`, override, onCheck, signal),
};

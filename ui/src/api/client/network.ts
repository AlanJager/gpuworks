import type { NetworkAttachBatchResult, NetworkAttachmentRequest, NetworkDetail, NetworkHCARequest, NetworkList, NetworkNodeFilters, NetworkOverrideRequest, NetworkRequest, NetworkTestEstimate, NetworkTestRequest, NetworkTestRun } from "../../networkTypes";
import { getJSON, sendJSON } from "../http";

export const networkApi = {
  networks: () => getJSON<NetworkList>("/api/v1/networks"),
  searchNetworkTargets: (id: number, kind: "nodes" | "attachments", q: string, signal?: AbortSignal, filters?: NetworkNodeFilters) => {
    const params = new URLSearchParams({ kind, q });
    for (const role of filters?.roles ?? []) params.append("role", role);
    for (const source of filters?.source ?? []) params.append("source", source);
    return getJSON<{ ids: string[] }>(`/api/v1/networks/${id}/search?${params}`, signal);
  },
  network: (id: number) => getJSON<NetworkDetail>(`/api/v1/networks/${id}`),
  createNetwork: (body: NetworkRequest) =>
    sendJSON<NetworkDetail>("POST", "/api/v1/networks", body),
  updateNetwork: (id: number, body: NetworkRequest) =>
    sendJSON<NetworkDetail>("PUT", `/api/v1/networks/${id}`, body),
  deleteNetwork: (id: number) =>
    sendJSON<{ status: string }>("DELETE", `/api/v1/networks/${id}`),
  attachNetwork: (id: number, body: NetworkAttachmentRequest) =>
    sendJSON<NetworkDetail>("POST", `/api/v1/networks/${id}/attachments`, body),
  attachNetworkNodes: (id: number, nodeUUIDs: string[]) =>
    sendJSON<NetworkAttachBatchResult>("POST", `/api/v1/networks/${id}/attachments/batch`, { node_uuids: nodeUUIDs }),
  editNetworkHCAs: (id: number, body: NetworkHCARequest) =>
    sendJSON<NetworkDetail>("POST", `/api/v1/networks/${id}/hcas`, body),
  updateNetworkAttachment: (id: number, attachmentID: number, body: NetworkOverrideRequest) =>
    sendJSON<NetworkDetail>("PUT", `/api/v1/networks/${id}/attachments/${attachmentID}`, body),
  updateNetworkLabel: (id: number, attachmentID: number, body: { label_key: string; label_value: string }) =>
    sendJSON<NetworkDetail>("PUT", `/api/v1/networks/${id}/attachments/${attachmentID}/label`, body),
  detachNetwork: (id: number, attachmentID: number) =>
    sendJSON<NetworkDetail>("DELETE", `/api/v1/networks/${id}/attachments/${attachmentID}`),
  startNetworkTest: (id: number, body: NetworkTestRequest) =>
    sendJSON<NetworkTestRun>("POST", `/api/v1/networks/${id}/tests`, body),
  estimateNetworkTest: (id: number, body: NetworkTestRequest) =>
    sendJSON<NetworkTestEstimate>("POST", `/api/v1/networks/${id}/tests/estimate`, body),
};

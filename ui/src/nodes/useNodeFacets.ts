import { api, usePolling, type NodeFacets, type NodeFilter } from "../api";

/**
 * useNodeFacets is what the server says about the nodes a filter matches:
 * their count and the labels and accelerators they carry. Pass null to skip
 * the request (a closed form).
 */
export function useNodeFacets(filter: NodeFilter | null, intervalMs = 0) {
  const key = filter ? JSON.stringify([filter.q ?? "", filter.state ?? [], filter.label ?? []]) : "";
  return usePolling<NodeFacets | null>(() => (filter ? api.nodeFacets(filter) : Promise.resolve(null)), filter ? intervalMs : 0, [key]);
}

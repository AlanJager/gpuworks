import { getJSON, type List } from "../http";
import type { EngineProfile, RoutingPolicyOption } from "../engines";

export const enginesApi = {
  engines: (targetVendor?: string) => getJSON<List<EngineProfile>>(`/api/v1/engines${targetVendor ? `?target_vendor=${encodeURIComponent(targetVendor)}` : ""}`),
  routingPolicies: () => getJSON<List<RoutingPolicyOption>>("/api/v1/routing-policies"),
};

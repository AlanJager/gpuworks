import { getJSON, sendJSON } from "./api";

export interface OutboundPolicy {
  restrict_private_hosts: boolean;
  allow_private_hosts: string[];
  editable: boolean;
  revision: string;
}

export const outboundPolicyApi = {
  get: () => getJSON<OutboundPolicy>("/api/v1/server/outbound-policy"),
  put: (value: Pick<OutboundPolicy, "restrict_private_hosts" | "allow_private_hosts" | "revision">) =>
    sendJSON<OutboundPolicy>("PUT", "/api/v1/server/outbound-policy", value),
};

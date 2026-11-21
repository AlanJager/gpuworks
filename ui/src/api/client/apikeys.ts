import { getJSON, sendJSON, type List } from "../http";
import type { ApiKey, ApiKeyPriority, ApiKeyScope, MCPStatus, PlatformToken, PriorityClass, PriorityClasses } from "../apikeys";

export const apikeysApi = {
  apiKeys: () => getJSON<List<ApiKey>>("/api/v1/apikeys"),
  createApiKey: (name: string, scope?: ApiKeyScope) =>
    sendJSON<{ key: string; name: string }>("POST", "/api/v1/apikeys", { name, ...scope }),
  setApiKeyScope: (prefix: string, scope: ApiKeyScope) =>
    sendJSON<ApiKey>("PUT", `/api/v1/apikeys/${prefix}/scope`, scope),
  revokeApiKey: (prefix: string) =>
    sendJSON<void>("DELETE", `/api/v1/apikeys/${prefix}`),
  setApiKeyPriority: (prefix: string, priority: ApiKeyPriority) =>
    sendJSON<ApiKey>("PUT", `/api/v1/apikeys/${prefix}/priority`, priority),
  priorityClasses: () => getJSON<PriorityClasses>("/api/v1/priority-classes"),
  createPriorityClass: (body: { name: string; display_name: string; description: string; value: number }) =>
    sendJSON<PriorityClass>("POST", "/api/v1/priority-classes", body),
  updatePriorityClass: (name: string, body: { display_name?: string; description?: string; value?: number }) =>
    sendJSON<PriorityClass>("PUT", `/api/v1/priority-classes/${encodeURIComponent(name)}`, body),
  deletePriorityClass: (name: string) =>
    sendJSON<void>("DELETE", `/api/v1/priority-classes/${encodeURIComponent(name)}`),
  setDefaultPriorityClass: (name: string) =>
    sendJSON<PriorityClasses>("PUT", `/api/v1/priority-classes/${encodeURIComponent(name)}/default`),
  platformTokens: () => getJSON<List<PlatformToken>>("/api/v1/platform-tokens"),
  createPlatformToken: (body: { name: string; scope: PlatformToken["scope"]; expires_in_days?: number }) =>
    sendJSON<{ token: string; item: PlatformToken }>("POST", "/api/v1/platform-tokens", body),
  deletePlatformToken: (id: number) => sendJSON<void>("DELETE", `/api/v1/platform-tokens/${id}`),
  mcpStatus: () => getJSON<MCPStatus>("/api/v1/mcp/status"),
  putMCPSettings: (enabled: boolean) => sendJSON<MCPStatus>("PUT", "/api/v1/mcp/settings", { enabled }),
};

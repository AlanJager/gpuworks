import { getJSON, sendJSON } from "../http";
import type { Branding, SecuritySettings, ServerListen, ServerListenUpdate } from "../settings";

export const settingsApi = {
  settings: () => getJSON<Record<string, any>>("/api/v1/settings"),
  putSetting: (key: string, value: unknown) =>
    sendJSON<void>("PUT", `/api/v1/settings/${key}`, value),
  serverListen: () => getJSON<ServerListen>("/api/v1/server/listen"),
  updateServerListen: (body: ServerListenUpdate) => sendJSON<ServerListen>("PUT", "/api/v1/server/listen", body),
  restartServer: () => sendJSON<{ status: string }>("POST", "/api/v1/server/restart"),
  branding: () => getJSON<Branding>("/api/v1/branding"),
  updateBranding: (body: Partial<Branding>) => sendJSON<Branding>("PATCH", "/api/v1/branding", body),
  securitySettings: () => getJSON<SecuritySettings>("/api/v1/security/settings"),
  updateSecuritySettings: (body: Partial<SecuritySettings>) => sendJSON<SecuritySettings>("PATCH", "/api/v1/security/settings", body),
};

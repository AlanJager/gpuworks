import { localizedResponseError, sendJSON } from "../http";
import type { NoticeList, ServerUpgradeStarted, ServerVersionChoice, UpdateStatus, UsageReportStatus } from "../updates";
import { sendFile } from "./transport";

export const updatesApi = {
  // The sign-in page asks the server to start its update check, so the header
  // has the answer once an administrator signs in. It needs no session and
  // the server answers nothing about the result.
  prefetchUpdates: async (): Promise<void> => {
    await fetch("/api/v1/updates/prefetch", { method: "POST" });
  },
  // The server reuses its last check for up to an hour; refresh asks the
  // distribution channel again.
  updates: (refresh = false) =>
    sendJSON<UpdateStatus>("GET", refresh ? "/api/v1/updates?refresh=true" : "/api/v1/updates"),
  setUpdateChannel: (channel: string) =>
    sendJSON<{ channel: string }>("PUT", "/api/v1/updates/channel", { channel }),
  setUpdateAutoCheck: (enabled: boolean) =>
    sendJSON<{ enabled: boolean }>("PUT", "/api/v1/updates/auto-check", { enabled }),
  usageReport: () => sendJSON<UsageReportStatus>("GET", "/api/v1/usage-report"),
  setUsageReport: (enabled: boolean) =>
    sendJSON<UsageReportStatus>("PUT", "/api/v1/usage-report/enabled", { enabled }),
  // The next report exactly as the server would send it, kept as text so the
  // console shows its bytes rather than a re-serialization.
  usageReportPreview: async (): Promise<string> => {
    const res = await fetch("/api/v1/usage-report/preview", { credentials: "include" });
    if (!res.ok) throw await localizedResponseError(res, `/api/v1/usage-report/preview: ${res.status}`);
    return res.text();
  },
  notices: () => sendJSON<NoticeList>("GET", "/api/v1/notices"),
  readNotices: (ids: string[]) => sendJSON<void>("POST", "/api/v1/notices/read", { ids }),
  // The server downloads its release from the channel, swaps the binary and restarts.
  serverVersions: (channel: string, signal?: AbortSignal) =>
    sendJSON<{ channel: string; platform: string; versions: ServerVersionChoice[] }>("GET",
      "/api/v1/updates/server/versions?channel=" + encodeURIComponent(channel), undefined, signal),
  serverUpgradeResult: (attempt: string, backup: string, signal: AbortSignal) =>
    sendJSON<{ phase: "unknown" | "download" | "restart" | "failed" | "rollback"; from?: string; to?: string }>("GET",
      "/api/v1/updates/server/result?" + new URLSearchParams({ attempt, backup }), undefined, signal),
  upgradeServer: (target?: { channel: string; version: string; sha256?: string; attempt: string }) =>
    sendJSON<ServerUpgradeStarted>("POST", "/api/v1/updates/server", target),
  // The same with a release archive uploaded for a server that cannot reach the channel.
  uploadServerArchive: (file: File) =>
    sendFile<ServerUpgradeStarted>("/api/v1/updates/server/upload", file),
};

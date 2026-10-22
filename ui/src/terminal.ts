// Web terminal client. Kept out of api.ts (already ~1000 lines) because this is
// a stateful WebSocket transport rather than another REST or SSE hook.
//
// Wire format, mirroring internal/server/api/terminal.go: terminal bytes travel
// as binary messages, structured control travels as JSON text. The byte-level
// framing to the agent lives on the server, so nothing here parses headers.

import { apiResponseError } from "./apiError";

export interface TerminalPreflight {
  enabled: boolean;
  idle_timeout_min: number;
  max_per_node: number;
  max_global: number;
  active_total: number;
  active_node: number;
  can_open: boolean;
  reason: string;
  container_supported: boolean;
  container_reason: string;
  /** Runtime selected for this node (for diagnostics and UI messaging). */
  container_runtime?: string;
  /** Capability bit required for the selected runtime's attach semantics. */
  container_capability?: string;
}

export interface TerminalInfo {
  user: string;
  uid: number;
  shell: string;
  cwd: string;
  host?: string;
}

export type TerminalTarget = "host" | "container";

// A browser cannot read the HTTP status of a failed WebSocket handshake, so the
// reason a terminal is unavailable has to be fetched before connecting.
export async function terminalPreflight(nodeUUID: string): Promise<TerminalPreflight> {
  const res = await fetch(`/api/v1/nodes/${encodeURIComponent(nodeUUID)}/terminal/settings`, {
    credentials: "include",
  });
  if (!res.ok) {
    throw await apiResponseError(res, res.status === 403 ? "Administrator permission is required" : `Unable to get terminal status (${res.status})`);
  }
  return res.json();
}

export interface TerminalSocketOptions {
  nodeUUID: string;
  target?: TerminalTarget;
  ref?: string;
  rows: number;
  cols: number;
  onData: (bytes: Uint8Array) => void;
  onInfo?: (info: TerminalInfo) => void;
  onExit?: (code: number, reason: string) => void;
  onError?: (message: string) => void;
  onClose?: (wasClean: boolean) => void;
}

export interface TerminalSocket {
  send: (data: string) => void;
  resize: (rows: number, cols: number) => void;
  close: () => void;
  readonly connected: boolean;
}

function socketURL(o: TerminalSocketOptions): string {
  const scheme = window.location.protocol === "https:" ? "wss:" : "ws:";
  const params = new URLSearchParams({
    target: o.target ?? "host",
    rows: String(o.rows),
    cols: String(o.cols),
  });
  if (o.ref) params.set("ref", o.ref);
  return `${scheme}//${window.location.host}/api/v1/nodes/${encodeURIComponent(o.nodeUUID)}/terminal?${params}`;
}

// openTerminalSocket connects and wires the callbacks. Authentication rides on
// the session cookie, which the browser attaches to a same-origin handshake, so
// no token appears in the URL where it would land in history and proxy logs.
export function openTerminalSocket(o: TerminalSocketOptions): TerminalSocket {
  const ws = new WebSocket(socketURL(o));
  ws.binaryType = "arraybuffer";

  const encoder = new TextEncoder();
  let open = false;
  let exited = false;

  ws.onopen = () => {
    open = true;
  };

  ws.onmessage = (ev: MessageEvent) => {
    if (typeof ev.data === "string") {
      let msg: Record<string, unknown>;
      try {
        msg = JSON.parse(ev.data);
      } catch {
        return;
      }
      if (msg.type === "info") {
        o.onInfo?.({
          user: String(msg.user ?? ""),
          uid: Number(msg.uid ?? -1),
          shell: String(msg.shell ?? ""),
          cwd: String(msg.cwd ?? ""),
          host: msg.host ? String(msg.host) : undefined,
        });
      } else if (msg.type === "exit") {
        exited = true;
        o.onExit?.(Number(msg.code ?? 0), String(msg.reason ?? ""));
      } else if (msg.type === "error") {
        o.onError?.(String(msg.message ?? "Terminal error"));
      }
      return;
    }
    o.onData(new Uint8Array(ev.data as ArrayBuffer));
  };

  ws.onerror = () => {
    // The event carries no detail by design; the preflight is what explains why
    // a terminal could not be opened.
    if (!exited) o.onError?.("Connection interrupted");
  };

  ws.onclose = (ev: CloseEvent) => {
    open = false;
    void fetch("/api/v1/auth/me", { credentials: "include" })
      .then(async (response) => { if (!response.ok) await apiResponseError(response, ""); })
      .catch(() => { /* A closed terminal does not establish that authentication expired. */ });
    o.onClose?.(ev.wasClean);
  };

  return {
    send(data: string) {
      if (ws.readyState === WebSocket.OPEN) ws.send(encoder.encode(data));
    },
    resize(rows: number, cols: number) {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: "resize", rows, cols }));
      }
    },
    close() {
      if (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING) {
        ws.close();
      }
    },
    get connected() {
      return open;
    },
  };
}

// TerminalSettings mirrors store.TerminalSettings. Reading and writing it goes
// through the shared api.settings() / api.putSetting() helpers rather than a
// second fetch wrapper here.
export interface TerminalSettings {
  enabled: boolean;
  idle_timeout_min: number;
  grace_sec: number;
  max_per_node: number;
  max_global: number;
}

export const defaultTerminalSettings: TerminalSettings = {
  enabled: true,
  idle_timeout_min: 15,
  grace_sec: 6,
  max_per_node: 4,
  max_global: 32,
};

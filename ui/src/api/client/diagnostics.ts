import type { DebugSessionStart, DebugSessionState, DiagnosticsBundle, DiagnosticsBundleStart, DiagnosticsBundles } from "../../diagnosticsApi";
import { getJSON, sendJSON } from "../http";

export const diagnosticsApi = {
  debugSession: () => getJSON<DebugSessionState>("/api/v1/diagnostics/debug-session"),
  startDebugSession: (body: DebugSessionStart) => sendJSON<DebugSessionState>("POST", "/api/v1/diagnostics/debug-session", body),
  stopDebugSession: () => sendJSON<DebugSessionState>("DELETE", "/api/v1/diagnostics/debug-session"),
  diagnosticsBundles: () => getJSON<DiagnosticsBundles>("/api/v1/diagnostics/bundles"),
  startDiagnosticsBundle: (body: DiagnosticsBundleStart) => sendJSON<DiagnosticsBundle>("POST", "/api/v1/diagnostics/bundles", body),
};

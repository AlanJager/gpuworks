import type { TelemetryExport, TelemetryExportTest, TelemetryExportUpdate } from "../../telemetryApi";
import { getJSON, sendJSON } from "../http";

export const telemetryApi = {
  telemetryExport: () => getJSON<TelemetryExport>("/api/v1/telemetry/export"),
  updateTelemetryExport: (body: TelemetryExportUpdate) => sendJSON<TelemetryExport>("PUT", "/api/v1/telemetry/export", body),
  testTelemetryExport: (body: TelemetryExportUpdate) => sendJSON<TelemetryExportTest>("POST", "/api/v1/telemetry/export/test", body),
};

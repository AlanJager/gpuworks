// Wire types of the request trace export (dto/telemetry.go):
// GET/PUT /api/v1/telemetry/export and POST /api/v1/telemetry/export/test.

export interface TelemetryExportStatus {
  active: boolean;
  traces_url: string;
  // Since the server started: spans the collector accepted, lost before
  // sending (the queue was full), refused or undeliverable.
  exported: number;
  dropped: number;
  failed: number;
  queued: number;
  last_error: string;
  last_error_at: number | null;
  last_export_at: number | null;
  // Why the stored setting could not be applied; "" when it was.
  setting_error: string;
}

export interface TelemetryExport {
  enabled: boolean;
  endpoint: string;
  // Stored headers by name; their values never come back.
  headers: { name: string }[];
  sample_ratio: number;
  trace_link: string;
  status: TelemetryExportStatus;
}

// A header of an update: a null value keeps the value stored under the name.
export interface TelemetryHeaderUpdate {
  name: string;
  value: string | null;
}

export interface TelemetryExportUpdate {
  enabled: boolean;
  endpoint: string;
  headers: TelemetryHeaderUpdate[];
  sample_ratio: number;
  trace_link: string;
}

export interface TelemetryExportTest {
  ok: boolean;
  trace_id: string;
  traces_url: string;
  error: string;
}

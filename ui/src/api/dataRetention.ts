/** How long each kind of data is kept (GET/PUT /api/v1/data-retention). */
export interface DataRetention {
  metrics_hours: number;
  logs_hours: number;
  request_log_days: number;
  request_log_max_mb: number;
  usage_hourly_days: number;
  usage_daily_days: number;
  stats_hourly_days: number;
  stats_daily_days: number;
  quality: boolean;
  gpu: boolean;
  gpu_time: boolean;
  time_zone: string;
}

/** The experience environment beside this server: a separate console holding only simulated data. */
export interface ExperienceStatus {
  enabled: boolean;
  ready: boolean;
  port: number;
  user: string;
  password?: string;
  error?: string;
}

export interface DataRetentionView {
  value: DataRetention;
  /** Bounds of every numeric value, by field name. */
  limits: Record<string, { min: number; max: number }>;
  server_time_zone: string;
}

export interface DataRetentionStore {
  kind: "metrics" | "logs" | "requests" | "usage" | "quality" | "gpu" | "gpu_time" | "scheduling";
  file: string;
  file_bytes: number | null;
  hourly_rows: number | null;
  daily_rows: number | null;
  earliest_ms: number | null;
}

export interface DataRetentionStatus {
  stores: DataRetentionStore[];
  live_since_ms: number;
  backfill: { usage_done: boolean; usage_through_ms: number; quality_done: boolean; quality_through_ms: number };
  retain: { at_ms: number; error?: string };
  rollup: { at_ms: number; error?: string };
  usage_flush: { at_ms: number; rows: number; ms: number; error?: string };
}

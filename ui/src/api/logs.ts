import type { CursorPage } from "./http";

export interface LogEntry {
  id: number;
  instance: string;
  node: string;
  level: string;
  source: string;
  time_ms: number;
  line: string;
}

export interface LogsFilter {
  q?: string;
  instance?: string;
  // instances is the service page's merged log stream: a comma-separated
  // list on the wire (max 64), mutually exclusive with `instance` - the
  // server rejects both being set at once.
  instances?: string[];
  node?: string;
  level?: string;
  source?: string;
  from?: number;
  to?: number;
  cursor_ts?: number;
  cursor_id?: number;
  limit?: number;
  with_total?: boolean;
}

// LogsPage is one newest-first page of GET /logs/search; total,
// total_capped and truncated are omitted when zero/false.
export interface LogsPage extends CursorPage<LogEntry> {
  total?: number;
  total_capped?: boolean;
  truncated?: boolean;
}

export interface LogsBucket {
  start_ms: number;
  counts: Record<string, number>;
}

export interface LogsWindow {
  items: LogEntry[];
  // Index of the anchor row in items, or -1 when the anchor is gone from
  // that partition (pruned by retention, or ts from another day). The
  // surrounding rows are still real, so compare against the index for
  // highlighting and never use it to index items without a >= 0 check.
  match_index: number;
}

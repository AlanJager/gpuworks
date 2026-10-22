import type { MonitorDistributions, MonitorGridCell, MonitorRequest, SegmentKind } from "../../monitorRequestsApi";

// Labels and segment arithmetic for the request-shape cards and the request
// view. Pure functions, so the numbers on screen are tested apart from the
// rendering.

// formatTokens writes a token count the way the axes do: 512, 2K, 1.5M.
export function formatTokens(n: number): string {
  if (n >= 1 << 20) return `${trim(n / (1 << 20))}M`;
  if (n >= 1024) return `${trim(n / 1024)}K`;
  return String(Math.round(n));
}

function trim(v: number): string {
  return v >= 10 ? String(Math.round(v)) : String(Math.round(v * 10) / 10);
}

export function formatMS(ms: number): string {
  if (ms >= 60_000) return `${trim(ms / 60_000)} min`;
  if (ms >= 1000) return `${trim(ms / 1000)} s`;
  return `${Math.round(ms)} ms`;
}

// tokenBucketLabel names token bucket i by its range: "1K–2K", the last one
// "≥ 1M".
export function tokenBucketLabel(bounds: number[], i: number): string {
  const low = bounds[i] ?? 0;
  const high = bounds[i + 1];
  if (high == null) return `≥ ${formatTokens(low)}`;
  return `${formatTokens(low)}–${formatTokens(high)}`;
}

// ttftBucketLabel names first-token bucket i: "< 100 ms", "100–250 ms",
// "> 300 s" after the last bound.
export function ttftBucketLabel(bounds: number[], i: number): string {
  if (i === 0) return `< ${formatMS(bounds[0] ?? 0)}`;
  if (i >= bounds.length) return `> ${formatMS(bounds[bounds.length - 1] ?? 0)}`;
  return `${formatMS(bounds[i - 1])}–${formatMS(bounds[i])}`;
}

// GridView is a distribution trimmed to the buckets that hold requests, so a
// cluster whose prompts all sit between 1K and 32K does not draw twenty empty
// columns.
export interface GridView {
  xs: number[];
  ys: number[];
  counts: Map<string, number>;
  total: number;
  max: number;
}

export function gridView(cells: MonitorGridCell[]): GridView {
  const counts = new Map<string, number>();
  let total = 0;
  let max = 0;
  let minX = Infinity;
  let maxX = -Infinity;
  let minY = Infinity;
  let maxY = -Infinity;
  for (const cell of cells) {
    if (cell.n <= 0) continue;
    const key = `${cell.x}:${cell.y}`;
    const n = (counts.get(key) ?? 0) + cell.n;
    counts.set(key, n);
    total += cell.n;
    max = Math.max(max, n);
    minX = Math.min(minX, cell.x);
    maxX = Math.max(maxX, cell.x);
    minY = Math.min(minY, cell.y);
    maxY = Math.max(maxY, cell.y);
  }
  if (total === 0) return { xs: [], ys: [], counts, total, max };
  const range = (a: number, b: number) => Array.from({ length: b - a + 1 }, (_, i) => a + i);
  return { xs: range(minX, maxX), ys: range(minY, maxY), counts, total, max };
}

// shareOf is a cell's share of the requests, as a rounded percentage string.
export function shareOf(n: number, total: number): string {
  if (total <= 0 || n <= 0) return "0%";
  const pct = (n / total) * 100;
  return pct >= 10 ? `${Math.round(pct)}%` : pct >= 0.1 ? `${pct.toFixed(1)}%` : "<0.1%";
}

// coverageShare is the share of requests a grid places, for its caption.
export function coverageShare(dist: MonitorDistributions, field: "with_usage" | "with_cache"): string {
  return shareOf(dist.coverage[field], dist.coverage.total);
}

// A request's phases, in order, as durations in milliseconds: the server's
// split (uxrecord.SegmentsOf), which the latency breakdown sums too, so a
// row's bar and the breakdown cannot disagree on where a wait belongs.
export type { SegmentKind };

export interface Segment {
  kind: SegmentKind;
  ms: number;
}

export interface Phases {
  segments: Segment[];
  totalMS: number;
  // firstTokenMS is the wait for the first output (the end, for a reply
  // without one); null while a live request still waits.
  firstTokenMS: number | null;
}

function known(v: number | null | undefined): v is number {
  return v != null && v >= 0;
}

export function requestPhases(r: MonitorRequest): Phases {
  const segments = (r.segments ?? []).filter((s) => s.ms > 0);
  const totalMS = segments.reduce((sum, s) => sum + s.ms, 0);
  const firsts = [r.first_think_ms, r.first_answer_ms].filter(known);
  const firstToken = firsts.length > 0 ? Math.min(...firsts) : null;
  return { segments, totalMS, firstTokenMS: firstToken ?? (r.phase === "done" ? totalMS : null) };
}

// Speeds the hover card quotes, in tokens per second; null when a figure it
// needs is missing.
export interface Speeds {
  prefillTokS: number | null;
  decodeTokS: number | null;
  uncached: number | null;
}

export function requestSpeeds(r: MonitorRequest): Speeds {
  const uncached = r.prompt_tokens != null ? Math.max(0, r.prompt_tokens - (r.cached_tokens ?? 0)) : null;
  // A PD handoff's prompt is computed by the prefill instance; the decode
  // instance only recomputes the last token (uxrecord.UncachedComputeMS).
  const computeMS = r.pd && !r.pd.fallback ? r.pd.prefill_compute_ms : r.engine_prefill_ms;
  const prefillTokS = uncached != null && known(computeMS) && computeMS > 0 ? (uncached * 1000) / computeMS : null;
  let decodeTokS: number | null = null;
  if (r.completion_tokens != null && r.completion_tokens > 1) {
    if (known(r.engine_decode_ms) && r.engine_decode_ms > 0) {
      decodeTokS = (r.completion_tokens * 1000) / r.engine_decode_ms;
    } else {
      const first = [r.first_think_ms, r.first_answer_ms].filter(known);
      if (first.length > 0 && known(r.last_chunk_ms)) {
        const span = r.last_chunk_ms - Math.min(...first);
        // The first token arrived at first, so the rest took the span.
        if (span > 0) decodeTokS = ((r.completion_tokens - 1) * 1000) / span;
      }
    }
  }
  return { prefillTokS, decodeTokS, uncached };
}

// liveEventRate is a live stream's events per second since its first token:
// the speed the gateway can see before the engine reports tokens.
export function liveEventRate(r: MonitorRequest): number | null {
  const first = [r.first_think_ms, r.first_answer_ms].filter(known);
  if (first.length === 0 || !known(r.last_chunk_ms) || r.events < 2) return null;
  const span = r.last_chunk_ms - Math.min(...first);
  return span > 0 ? ((r.events - 1) * 1000) / span : null;
}

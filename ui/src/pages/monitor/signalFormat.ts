// Values arrive already rounded server-side; this only enforces the display
// contract (at most one decimal, never a trailing ".0") against
// floating-point noise from client-side formatting - e.g. 2 stays "2", not
// "2.0", and 2.35 becomes "2.4", not "2.35" or "2.3999999999999995".
export function formatSignalValue(value: number): string {
  const rounded = Math.round(value * 10) / 10;
  return Number.isInteger(rounded) ? String(rounded) : rounded.toFixed(1);
}

// latencyUnit picks the unit a latency is shown in: seconds from 1 s up, so a
// 39000 ms TTFT reads "39" s instead of a five-digit millisecond count. A
// p95/p99 pair shares one unit, picked by its larger value.
export function latencyUnit(...ms: (number | null | undefined)[]): "ms" | "s" {
  return Math.max(0, ...ms.map((v) => v ?? 0)) >= 1000 ? "s" : "ms";
}

export function formatLatency(ms: number, unit: "ms" | "s"): string {
  return formatSignalValue(unit === "s" ? ms / 1000 : ms);
}

// formatLatencyText is a latency with its unit attached, for places with no
// separate unit slot: "39s", "345.6 ms".
export function formatLatencyText(ms: number): string {
  const unit = latencyUnit(ms);
  return unit === "s" ? `${formatLatency(ms, unit)}s` : `${formatLatency(ms, unit)} ms`;
}

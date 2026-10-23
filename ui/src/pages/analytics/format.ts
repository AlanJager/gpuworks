// Number formatting shared by the analytics tabs. A missing value is a dash,
// never a zero: a statistic nobody measured is not a statistic of zero.
export const DASH = "—";

export function fmtInt(n: number | null | undefined): string {
  return n == null ? DASH : Math.round(n).toLocaleString("en-US");
}

/** Compact in the reader's language: 1.23万 / 1.23M. The exact figure belongs in a title attribute. */
export function fmtCompact(n: number | null | undefined, language: string): string {
  if (n == null) return DASH;
  try {
    return new Intl.NumberFormat(language, { notation: "compact", maximumFractionDigits: 2 }).format(n);
  } catch {
    return fmtInt(n);
  }
}

/** a over b as a percentage; null when b is not positive (nothing to divide by). */
export function ratio(a: number, b: number): number | null {
  return b > 0 ? (a / b) * 100 : null;
}

export function fmtPct(p: number | null | undefined, digits = 1): string {
  return p == null || Number.isNaN(p) ? DASH : `${p.toFixed(digits)}%`;
}

export function fmtFixed(n: number | null | undefined, digits = 1): string {
  return n == null ? DASH : n.toFixed(digits);
}

/** Milliseconds as seconds with two decimals; null stays a dash. */
export function fmtSeconds(ms: number | null | undefined): string {
  return ms == null ? DASH : `${(ms / 1000).toFixed(2)} s`;
}

export function fmtWatts(w: number | null | undefined): string {
  return w == null ? DASH : `${Math.round(w)} W`;
}

export function fmtCelsius(c: number | null | undefined): string {
  return c == null ? DASH : `${Math.round(c)} °C`;
}

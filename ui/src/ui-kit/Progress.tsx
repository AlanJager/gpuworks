/**
 * The fill follows the outcome it reports: brand while things go as expected,
 * warning or danger when the caller says some of what it counts went wrong.
 * The tone is the caller's statement, never inferred from the value.
 */
export type ProgressTone = "brand" | "warning" | "danger";

const fills: Record<ProgressTone, string> = { brand: "bg-brand", warning: "bg-warning-strong", danger: "bg-danger-strong" };

export function Progress({ value, max = 100, label, tone = "brand" }: { value: number; max?: number; label: string; tone?: ProgressTone }) {
  const limit = max > 0 ? max : 100;
  const bounded = Math.min(limit, Math.max(0, Number.isFinite(value) ? value : 0));
  return <div role="progressbar" aria-label={label} aria-valuemin={0} aria-valuemax={limit} aria-valuenow={bounded}
    data-tone={tone} className="h-2 w-full overflow-hidden rounded-full bg-emphasis">
    <div className={`h-full rounded-full ${fills[tone]} transition-[width] duration-moderate motion-reduce:transition-none`} style={{ width: `${bounded / limit * 100}%` }} />
  </div>;
}

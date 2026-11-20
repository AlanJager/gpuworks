import { useTranslation } from "react-i18next";
import { Button } from "../components/ui/button";
import { useIcon } from "@/lib/icon-context";
import { cn } from "@/lib/utils";

/**
 * A pager for a list whose page size the server fixes (no size select, unlike
 * Zeron's list-pagination): the total, "page n of m" and first / previous /
 * next / last. `page` is one-based, as the list APIs count it; the caller
 * fetches the page.
 */
export function Pager({ page, pageSize, total, onChange, className }: { page: number; pageSize: number; total: number; onChange: (page: number) => void; className?: string }) {
  const { t } = useTranslation("common");
  const First = useIcon("chevrons-left");
  const Previous = useIcon("chevron-left");
  const Next = useIcon("chevron-right");
  const Last = useIcon("chevrons-right");
  const count = Math.max(1, Math.ceil(Math.max(0, total) / Math.max(1, pageSize)));
  const current = Math.min(Math.max(1, page), count);
  const go = (next: number) => onChange(Math.min(Math.max(1, next), count));
  return <nav aria-label={t("resourceTable.pageSummary", { current, count })} className={cn("flex flex-wrap items-center justify-end gap-3 py-1.5 text-label text-fg-muted", className)}>
    <span>{t("resourceTable.total", { count: total })}</span>
    <span className="tabular-nums">{t("resourceTable.pageSummary", { current, count })}</span>
    <span className="flex items-center gap-2">
      <Button aria-label={t("resourceTable.firstPage")} disabled={current === 1} iconOnly onClick={() => go(1)} size="sm" type="button" variant="tertiary"><First aria-hidden /></Button>
      <Button aria-label={t("resourceTable.previousPage")} disabled={current === 1} iconOnly onClick={() => go(current - 1)} size="sm" type="button" variant="tertiary"><Previous aria-hidden /></Button>
      <Button aria-label={t("resourceTable.nextPage")} disabled={current === count} iconOnly onClick={() => go(current + 1)} size="sm" type="button" variant="tertiary"><Next aria-hidden /></Button>
      <Button aria-label={t("resourceTable.lastPage")} disabled={current === count} iconOnly onClick={() => go(count)} size="sm" type="button" variant="tertiary"><Last aria-hidden /></Button>
    </span>
  </nav>;
}

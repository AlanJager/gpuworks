import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { CardHealthRecord, CardHealthRecords } from "../../api";
import EmptyState from "../EmptyState";
import { Pager } from "../../ui-kit/Pager";
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from "../ui/accordion";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import { Skeleton } from "../ui/skeleton";
import { cardName, checkName, codeText, durationText, localTime, statusName } from "./text";

// HealthRecords lists the stretches in which a check warned or failed: ongoing
// ones first (the server orders them), each with how long it lasted, how many
// runs found it, and what the first and latest of them read.
export default function HealthRecords({
  data,
  page,
  onPage,
  filter,
  onClearFilter,
  showCard = true,
}: {
  data?: CardHealthRecords;
  page: number;
  onPage: (page: number) => void;
  filter?: string;
  onClearFilter?: () => void;
  showCard?: boolean;
}) {
  const { t } = useTranslation("nodes");
  const days = data?.retention_days ?? 30;
  return (
    <div className="card-health-records">
      <div className="mb-2 flex w-full flex-wrap items-center justify-between gap-2">
        <span className="font-semibold">{t("cardHealth.records.title", { days })}</span>
        {filter && (
          <span className="inline-flex items-center gap-2">
            <span className="text-fg-muted">{filter}</span>
            <Button size="sm" variant="tertiary" onClick={onClearFilter}>
              {t("cardHealth.records.clear")}
            </Button>
          </span>
        )}
      </div>
      {!data ? (
        <div aria-busy="true" className="flex flex-col gap-3 py-2"><Skeleton className="h-4 w-3/5" /><Skeleton className="h-4 w-full" /><Skeleton className="h-4 w-2/5" /></div>
      ) : data.records.length === 0 ? (
        <EmptyState compact description={t("cardHealth.records.empty", { days })} />
      ) : (
        <ul className="m-0 flex list-none flex-col divide-y divide-border-subtle p-0">
          {data.records.map((r) => <Record key={r.id} record={r} days={days} showCard={showCard} />)}
        </ul>
      )}
      {data && data.total > data.page_size && (
        <Pager className="mt-2" page={page} pageSize={data.page_size} total={data.total} onChange={onPage} />
      )}
    </div>
  );
}

function Record({ record: r, days, showCard }: { record: CardHealthRecord; days: number; showCard: boolean }) {
  const { t } = useTranslation("nodes");
  const [open, setOpen] = useState<string[]>([]);
  const span = days * 86400;
  const end = r.ended_at ? new Date(r.ended_at).getTime() : Date.now();
  const startOffset = Math.max(0, Math.min(1, 1 - (Date.now() - new Date(r.started_at).getTime()) / 1000 / span));
  const width = Math.max(0.005, Math.min(1 - startOffset, (end - new Date(r.started_at).getTime()) / 1000 / span));
  const evidence = [
    { key: "first", label: t("cardHealth.records.first"), fields: r.first_evidence },
    ...(r.latest_evidence.length && JSON.stringify(r.latest_evidence) !== JSON.stringify(r.first_evidence)
      ? [{ key: "latest", label: t("cardHealth.records.latest"), fields: r.latest_evidence }]
      : []),
  ].filter((e) => e.fields.length);
  return (
    <li className="py-3">
      <span className="inline-flex flex-wrap items-center gap-2">
        {r.ongoing && <Badge size="sm" status="danger">{t("cardHealth.records.ongoing")}</Badge>}
        {r.flapping && <Badge size="sm" color="purple">{t("cardHealth.records.flapping")}</Badge>}
        {showCard && (
          <span className="font-semibold">
            {cardName(t, r.card_index, r.card_ident)}
            {r.removed && <Badge size="sm" className="ml-1.5">{t("cardHealth.removed")}</Badge>}
          </span>
        )}
        <span>{checkName(t, r.check)}</span>
        <Badge size="sm" status={r.status === "fail" ? "danger" : "warning"}>{codeText(t, r.status, r.code) || statusName(t, r.status)}</Badge>
        <span className="text-fg-muted">
          {localTime(r.started_at)}
          {r.ended_at ? ` – ${localTime(r.ended_at)}` : ""}
        </span>
        <span className="text-fg-muted">
          {t(r.ongoing ? "cardHealth.records.lasting" : "cardHealth.records.lasted", { duration: durationText(t, r.duration_seconds) })}
        </span>
        <span className="text-fg-muted">{t("cardHealth.records.count", { count: r.count })}</span>
      </span>
      <div aria-hidden className="relative mt-1.5 h-1 rounded-sm bg-emphasis">
        <div
          className={`absolute h-1 rounded-sm ${r.status === "fail" ? "bg-danger-strong" : "bg-warning-strong"}`}
          style={{ left: `${startOffset * 100}%`, width: `${width * 100}%` }}
        />
      </div>
      {evidence.length > 0 && (
        <Accordion type="multiple" className="mt-1" value={open} onValueChange={(value: string[]) => setOpen(value)}>
          {evidence.map((e) => (
            <AccordionItem key={e.key} value={e.key}>
              <AccordionTrigger>{e.label}</AccordionTrigger>
              <AccordionContent>
                {open.includes(e.key) && e.fields.map((f) => (
                  <div key={f.name}>
                    <span className="text-fg-muted">{f.name}</span> <span>{f.value}</span>
                  </div>
                ))}
              </AccordionContent>
            </AccordionItem>
          ))}
        </Accordion>
      )}
    </li>
  );
}

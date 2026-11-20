import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { CardRef } from "../../api";
import { agoText } from "../../format";
import { Notice } from "../../ui-kit/Notice";
import { InfoItem, InfoItemContent, InfoItemDescription, InfoItemGroup, InfoItemLeading, InfoItemTitle } from "../ui/info-item";
import { Skeleton } from "../ui/skeleton";
import CheckCell from "./CheckCell";
import CheckDetail from "./CheckDetail";
import HealthRecords from "./HealthRecords";
import { HealthBanner } from "./NodeHealthTab";
import { checkName, codeText, periodText, statusName } from "./text";
import { useCardHealthChecks, useCardHealthRecords } from "./useCardHealth";

// CardHealthTab is one card's health checks: each check on a line of its own
// on top, the card's failure records below. Choosing a check shows what it
// last did and narrows the records to it.
export default function CardHealthTab({ nodeUUID, card, cardLabel }: { nodeUUID: string; card: CardRef; cardLabel: string }) {
  const { t } = useTranslation("nodes");
  const { t: tCommon } = useTranslation("common");
  const checks = useCardHealthChecks(nodeUUID, card, tCommon);
  const [selected, setSelected] = useState<string>();
  const [page, setPage] = useState(1);
  const records = useCardHealthRecords(nodeUUID, card, selected, page, tCommon);
  const data = checks.data;
  const row = data?.cards[0];
  const periods = Object.fromEntries((data?.checks ?? []).map((c) => [c.id, c.period_seconds]));
  const select = (check?: string) => {
    setSelected(check);
    setPage(1);
  };
  return (
    <div className="flex w-full flex-col gap-4">
      {checks.error && <Notice tone="danger" title={checks.error} />}
      {!data && !checks.error && <div className="flex flex-col gap-3"><Skeleton className="h-4 w-2/5" /><Skeleton className="h-4 w-full" /><Skeleton className="h-4 w-full" /><Skeleton className="h-4 w-3/5" /></div>}
      {data && <HealthBanner data={data} />}
      {row && (
        <InfoItemGroup role="list">
          {row.cells.map((cell) => {
            const reason = codeText(t, cell.status, cell.code);
            const ago = cell.checked_at ? agoText(t, (Date.now() - new Date(cell.checked_at).getTime()) / 1000) : "";
            return (
              <InfoItem key={cell.check} role="listitem" className={cell.status === "na" ? "cursor-default" : "cursor-pointer"} onClick={() => cell.status !== "na" && select(selected === cell.check ? undefined : cell.check)}>
                <InfoItemLeading className="bg-transparent"><CheckCell cell={cell} selected={selected === cell.check} /></InfoItemLeading>
                <InfoItemContent>
                  <InfoItemTitle className="flex flex-wrap items-baseline gap-3">
                    <span className="font-semibold">{checkName(t, cell.check)}</span>
                    <span className="font-normal">{statusName(t, cell.stale ? "stale" : cell.status)}</span>
                  </InfoItemTitle>
                  {(reason || ago || !!cell.duration_ms || periods[cell.check]) && (
                    <InfoItemDescription className="flex flex-wrap gap-x-3">
                      {reason && <span>{reason}</span>}
                      {ago && <span>{t("cardHealth.lastChecked", { ago })}</span>}
                      {!!cell.duration_ms && <span>{`${cell.duration_ms} ms`}</span>}
                      {periods[cell.check] && <span>{periodText(t, periods[cell.check])}</span>}
                    </InfoItemDescription>
                  )}
                </InfoItemContent>
              </InfoItem>
            );
          })}
        </InfoItemGroup>
      )}
      {row && selected && (
        <CheckDetail
          key={selected}
          nodeUUID={nodeUUID}
          card={card}
          cardLabel={cardLabel}
          check={selected}
          canRecheck={row.can_recheck}
          onRechecked={() => {
            checks.reload();
            records.reload();
          }}
        />
      )}
      {records.error && <Notice tone="danger" title={records.error} />}
      <HealthRecords
        data={records.data}
        page={page}
        onPage={setPage}
        showCard={false}
        filter={selected ? checkName(t, selected) : undefined}
        onClearFilter={() => select(undefined)}
      />
    </div>
  );
}

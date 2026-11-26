import { Alert, Skeleton, Space, Tooltip, Typography } from "antd";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { CardHealthCard, CardHealthChecks } from "../../api";
import CheckCell from "./CheckCell";
import CheckDetail from "./CheckDetail";
import HealthRecords from "./HealthRecords";
import { checkHelp, checkName, periodText } from "./text";
import { useCardHealthChecks, useCardHealthRecords } from "./useCardHealth";

type Selection = { card: CardHealthCard; check: string };

const noticeKeys: Record<string, string> = {
  CARD_CHECKS_OFFLINE: "cardHealth.banner.CARD_CHECKS_OFFLINE",
  CARD_CHECKS_WAITING: "cardHealth.banner.CARD_CHECKS_WAITING",
  CARD_CHECKS_UNSUPPORTED: "cardHealth.banner.CARD_CHECKS_UNSUPPORTED",
  CARD_CHECKS_STALE: "cardHealth.banner.CARD_CHECKS_STALE",
  CARD_CHECKS_PASSTHROUGH: "cardHealth.banner.CARD_CHECKS_PASSTHROUGH",
  CARD_CHECKS_NO_DRIVER: "cardHealth.banner.CARD_CHECKS_NO_DRIVER",
  CARD_CHECKS_HELD_ELSEWHERE: "cardHealth.banner.CARD_CHECKS_HELD_ELSEWHERE",
  CARD_CHECKS_NO_USABLE_CARDS: "cardHealth.banner.CARD_CHECKS_NO_USABLE_CARDS",
  CARD_CHECKS_NO_CARDS: "cardHealth.banner.CARD_CHECKS_NO_CARDS",
};

/** Display the server's availability explanation without inferring capability. */
export function HealthBanner({ data }: { data: CardHealthChecks }) {
  const { t } = useTranslation("nodes");
  return data.notice ? <Alert type={data.notice.level} showIcon message={t(noticeKeys[data.notice.code] ?? "cardHealth.banner.unavailable")} /> : null;
}

// NodeHealthTab is the node's health checks: a table of cards by checks on
// top, the node's failure records below. Choosing a cell shows what that
// check last did and narrows the records to it.
export default function NodeHealthTab({ nodeUUID }: { nodeUUID: string }) {
  const { t } = useTranslation("nodes");
  const { t: tCommon } = useTranslation("common");
  const checks = useCardHealthChecks(nodeUUID, undefined, tCommon);
  const [selected, setSelected] = useState<Selection>();
  const [page, setPage] = useState(1);
  const filterCard = selected ? { pcie_bdf: selected.card.pcie_bdf, uuid: selected.card.uuid } : undefined;
  const records = useCardHealthRecords(nodeUUID, filterCard, selected?.check, page, tCommon);
  const data = checks.data;
  const label = (card: CardHealthCard) => t("cardHealth.card", { index: card.index });
  const select = (s?: Selection) => {
    setSelected(s);
    setPage(1);
  };
  const refresh = () => {
    checks.reload();
    records.reload();
  };

  return (
    <Space direction="vertical" size={16} style={{ width: "100%" }}>
      {checks.error && <Alert type="error" showIcon message={checks.error} />}
      {!data && !checks.error && <Skeleton active />}
      {data && (
        <>
          <HealthBanner data={data} />
          {data.cards.length > 0 && (
            <>
              <Typography.Text type="secondary">
                {t("cardHealth.summary", { cards: data.cards.length, fail: data.summary.fail ?? 0, warn: data.summary.warn ?? 0, skip: data.summary.skip ?? 0 })}
              </Typography.Text>
              <div style={{ overflowX: "auto" }}>
                <table className="card-health-table" style={{ borderCollapse: "separate", borderSpacing: 4 }}>
                  <thead>
                    <tr>
                      <th />
                      {data.checks.map((c) => (
                        <th key={c.id} scope="col" style={{ fontWeight: 500, fontSize: 12, padding: "0 4px", whiteSpace: "nowrap" }}>
                          <Tooltip title={`${checkHelp(t, c.id)} · ${periodText(t, c.period_seconds)}`}>{checkName(t, c.id)}</Tooltip>
                        </th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {data.cards.map((card) => (
                      <tr key={card.ident}>
                        <th scope="row" style={{ textAlign: "left", fontWeight: 400, whiteSpace: "nowrap", paddingRight: 8 }}>
                          {label(card)}
                          {card.name ? <Typography.Text type="secondary"> {card.name}</Typography.Text> : null}
                        </th>
                        {card.cells.map((cell) => (
                          <td key={cell.check} style={{ textAlign: "center" }}>
                            <CheckCell
                              cell={cell}
                              selected={selected?.card.ident === card.ident && selected.check === cell.check}
                              onSelect={() => select(selected?.card.ident === card.ident && selected.check === cell.check ? undefined : { card, check: cell.check })}
                            />
                          </td>
                        ))}
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </>
          )}
          {selected && (
            <CheckDetail
              key={`${selected.card.ident}/${selected.check}`}
              nodeUUID={nodeUUID}
              card={{ pcie_bdf: selected.card.pcie_bdf, uuid: selected.card.uuid }}
              cardLabel={label(selected.card)}
              check={selected.check}
              canRecheck={selected.card.can_recheck}
              onRechecked={refresh}
            />
          )}
        </>
      )}
      {records.error && <Alert type="error" showIcon message={records.error} />}
      <HealthRecords
        data={records.data}
        page={page}
        onPage={setPage}
        filter={selected ? t("cardHealth.records.filteredBy", { card: label(selected.card), check: checkName(t, selected.check) }) : undefined}
        onClearFilter={() => select(undefined)}
      />
    </Space>
  );
}

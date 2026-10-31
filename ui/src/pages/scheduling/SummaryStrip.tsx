import { Tag, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { SchedulingOverview } from "../../schedulingApi";
import { stateFill } from "./cellColor";
import { CELL_STATES, type CellState } from "./overviewModel";

const SUMMARY_KEY: Record<CellState, keyof SchedulingOverview["summary"]> = {
  awake: "awake", asleep: "asleep", free: "free", held_outside: "held_outside", unhealthy: "unhealthy", offline: "offline",
};

export interface QueueCounts {
  pending: number;
  upcoming: number;
}

function Tile({ label, value, hint, swatch }: { label: string; value: number | string; hint?: string; swatch?: string }) {
  return (
    <div className="scheduling-tile">
      <Typography.Text type="secondary">
        {swatch && <span className="hm-swatch" style={{ background: swatch, marginInlineEnd: 6 }} aria-hidden="true" />}
        {label}
      </Typography.Text>
      <strong className="scheduling-num">{value}</strong>
      {hint && <Typography.Text type="secondary" className="scheduling-num">{hint}</Typography.Text>}
    </div>
  );
}

/**
 * SummaryStrip is the fleet at a glance: how many cards there are and in what
 * state, by GPU model, and how hard the deployments push on each other for
 * them. The figures are the server's, over the whole fleet whatever the
 * filters below it show.
 */
export default function SummaryStrip({ summary, queues }: { summary: SchedulingOverview["summary"]; queues?: QueueCounts }) {
  const { t } = useTranslation("scheduling");
  const contention = summary.contention;
  return (
    <div className="hm-strip" role="group" aria-label={t("summary.title")}>
      <div className="hm-strip-tiles" role="group" aria-label={t("summary.cardsTitle")}>
        <Tile label={t("summary.total")} value={summary.cards} hint={t("summary.nodesOnline", { online: summary.online_nodes, count: summary.nodes })} />
        {CELL_STATES.filter((state) => state !== "unhealthy" || summary.unhealthy > 0).map((state) => (
          <Tile key={state} label={t(`states.${state}`)} value={summary[SUMMARY_KEY[state]] as number} swatch={stateFill[state]} />
        ))}
      </div>
      {summary.by_model.length > 0 && (
        <ul className="hm-models" aria-label={t("summary.models")}>
          {summary.by_model.map((row) => (
            <li key={row.model}><Tag>{t("summary.modelLine", { model: row.model, cards: row.cards, free: row.free })}</Tag></li>
          ))}
        </ul>
      )}
      <div className="hm-strip-tiles" role="group" aria-label={t("summary.contentionTitle")}>
        <Tile label={t("summary.waitingWakes")} value={contention.waiting_wakes} />
        <Tile label={t("summary.heldRequests")} value={contention.held_requests} />
        <Tile label={t("summary.yields")} value={contention.yields_last_hour} />
        <Tile label={t("summary.pending")} value={queues?.pending ?? "—"} />
        <Tile label={t("summary.upcoming")} value={queues?.upcoming ?? "—"} />
      </div>
    </div>
  );
}

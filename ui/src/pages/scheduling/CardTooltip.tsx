import { useTranslation } from "react-i18next";
import { formatBytes } from "../../format";
import type { CardView, OverviewModel } from "./overviewModel";

export interface ActiveCard {
  card: CardView;
  rect: DOMRect;
}

const WIDTH = 280;
const GAP = 8;

/**
 * CardTooltip is the one hover card of the heatmap: it follows the cell the
 * pointer or the keyboard is on. It repeats what the cell's accessible name
 * says, so it is hidden from assistive technology.
 */
export default function CardTooltip({ active, deployments }: { active: ActiveCard; deployments: OverviewModel["deployments"] }) {
  const { t } = useTranslation("scheduling");
  const { t: tDeploy } = useTranslation("deployments");
  const { card, rect } = active;
  const viewport = typeof window === "undefined" ? 1280 : window.innerWidth;
  const left = Math.max(Math.min(rect.left, viewport - WIDTH - GAP), GAP);
  const freePct = Math.max(100 - card.awakePct - card.sleepingPct, 0);
  return (
    <div className="hm-tip" role="tooltip" aria-hidden="true" style={{ left, top: rect.bottom + GAP, width: WIDTH }}>
      <strong>{t("heatmap.tipTitle", { node: card.node.name, index: card.index })}</strong>
      <div>{t("heatmap.tipModel", { model: card.model, memory: formatBytes(card.memoryMiB * 1024 * 1024) })}</div>
      <div>{t(`states.${card.state}`)}{card.switching ? ` · ${t("heatmap.detail.switching")}` : ""}</div>
      {card.state !== "offline" && (
        <div className="scheduling-num">{t("heatmap.tipShares", { awake: card.awakePct, sleeping: card.sleepingPct, free: freePct })}</div>
      )}
      {card.occupants.length > 0 && (
        <ul className="hm-tip-holders">
          {card.occupants.map((occupant) => {
            const dep = deployments[occupant.deployment];
            return (
              <li key={`${occupant.deployment}-${occupant.asleep ? "s" : "a"}`}>
                {t("heatmap.holder", {
                  name: dep?.name ?? "?",
                  level: tDeploy(`yieldPolicy.preset.name.${dep?.preset ?? "resident"}`),
                  state: t(`heatmap.holderState.${occupant.asleep ? "asleep" : "awake"}`),
                })}
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}

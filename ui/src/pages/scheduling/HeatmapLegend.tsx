import { useTranslation } from "react-i18next";
import { awakeFill, stateFill, tokenVars } from "./cellColor";
import { CELL_STATES } from "./overviewModel";

/**
 * HeatmapLegend names the colours once for the whole heatmap: a state each,
 * the awake blue deepening with the share of the card claimed, an outline for a
 * card changing state, and, while a deployment is searched, the ring on its cards.
 */
export default function HeatmapLegend({ searching }: { searching: boolean }) {
  const { t } = useTranslation("scheduling");
  return (
    <ul className="hm-legend" style={tokenVars()} aria-label={t("nodes.legend.title")}>
      {CELL_STATES.map((state) => (
        <li key={state}>
          <span className={`hm-swatch is-${state}`} style={{ background: state === "awake" ? awakeFill(100) : stateFill[state] }} aria-hidden="true" />
          {t(`states.${state}`)}
        </li>
      ))}
      <li>
        <span className="hm-swatch" style={{ background: `linear-gradient(90deg, ${awakeFill(1)}, ${awakeFill(100)})`, width: 28 }} aria-hidden="true" />
        {t("heatmap.legendShade")}
      </li>
      <li>
        <span className="hm-swatch is-switching" aria-hidden="true" />
        {t("heatmap.legendSwitching")}
      </li>
      {searching && (
        <li>
          <span className="hm-swatch is-hit" aria-hidden="true" />
          {t("heatmap.legendHit")}
        </li>
      )}
    </ul>
  );
}

import { useTranslation } from "react-i18next";
import type { MonitorService } from "../../api";
import { Button } from "../../components/ui/button";
import { Badge } from "../../components/ui/badge";
import { STATE_BADGE, STATE_ORDER } from "./serviceStates";

export default function StateSummary({ services, selected, onSelect }: {
  services: MonitorService[]; selected: string[]; onSelect: (state: MonitorService["state"]) => void;
}) {
  const { t } = useTranslation("monitoring");
  const counts = new Map<MonitorService["state"], number>();
  for (const service of services) counts.set(service.state, (counts.get(service.state) ?? 0) + 1);
  return <div className="flex flex-wrap items-center gap-1.5" role="group" aria-label={t("stateSummary.title")}>
    {STATE_ORDER.filter(state => counts.has(state)).map(state => <Button key={state} variant="ghost" size="sm"
      active={selected.includes(state)} aria-pressed={selected.includes(state)} onClick={() => onSelect(state)}>
      <Badge variant="plain" status={STATE_BADGE[state]}>{t("stateSummary.count", { state: t(`states.${state}`), count: counts.get(state) })}</Badge>
    </Button>)}
  </div>;
}

import { Badge, Button, Space, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { MonitorService } from "../../api";
import { STATE_BADGE, STATE_ORDER } from "./ServiceTable";

// StateSummary is the line under the signal row: how many services are in
// each state, each count a link that narrows the services table to them.
// Health check results stay a column (and column filter) of the table.
export default function StateSummary({ services, onFilter }: { services: MonitorService[]; onFilter: (deploymentIds: number[]) => void }) {
  const { t } = useTranslation("monitoring");
  const byState = new Map<MonitorService["state"], number[]>();
  for (const s of services) {
    byState.set(s.state, [...(byState.get(s.state) ?? []), s.deployment_id]);
  }
  return (
    <Space size={[16, 4]} wrap align="center">
      <Typography.Text>{t("stateSummary.title")}</Typography.Text>
      {STATE_ORDER.filter((state) => byState.has(state)).map((state) => (
        <Button key={state} type="link" size="small" style={{ padding: 0, height: "auto" }} onClick={() => onFilter(byState.get(state)!)}>
          <Badge status={STATE_BADGE[state]} />
          {t("stateSummary.count", { state: t(`states.${state}`), count: byState.get(state)!.length })}
        </Button>
      ))}
    </Space>
  );
}

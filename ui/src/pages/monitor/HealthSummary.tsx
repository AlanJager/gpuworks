import { Button, Space, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { HealthStatus, MonitorService } from "../../api";
import { HealthCheckHelp } from "../../components/DeploymentHealth";
import { HEALTH_ORDER } from "./ServiceTable";

// HealthSummary is the line under the signal row: how many services are in
// each health check status, each count a link that narrows the services
// table to them. It counts the checks' findings and adds no verdict of its
// own.
export default function HealthSummary({ services, onFilter }: { services: MonitorService[]; onFilter: (deploymentIds: number[]) => void }) {
  const { t } = useTranslation("monitoring");
  const { t: tCommon } = useTranslation("common");
  const byStatus = new Map<HealthStatus, number[]>();
  for (const s of services) {
    byStatus.set(s.health.status, [...(byStatus.get(s.health.status) ?? []), s.deployment_id]);
  }
  const checked = services.length - (byStatus.get("off")?.length ?? 0);
  return (
    <Space size={[12, 4]} wrap align="center">
      <Typography.Text>
        {t("healthSummary.title")}
        <HealthCheckHelp />
      </Typography.Text>
      {checked === 0 ? (
        <Typography.Text type="secondary">{t("healthSummary.noneChecked")}</Typography.Text>
      ) : (
        HEALTH_ORDER.filter((status) => byStatus.has(status)).map((status) => (
          <Button key={status} type="link" size="small" style={{ padding: 0, height: "auto" }} onClick={() => onFilter(byStatus.get(status)!)}>
            {t("healthSummary.count", { status: tCommon(`healthCheck.status.${status}`), count: byStatus.get(status)!.length })}
          </Button>
        ))
      )}
    </Space>
  );
}

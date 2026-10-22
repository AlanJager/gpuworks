import { useState } from "react";
import { Button, InputNumber, Space, Switch, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type Deployment } from "../api";
import { HealthTag, healthCheckable } from "../components/DeploymentHealth";
import { useOrder } from "./useOrder";

// HealthCheckEditor is the deployment detail's health check: what it found,
// the switch and the threshold. A change takes effect at the next round of
// checks; nothing restarts.
export function HealthCheckEditor({
  dep,
  onChanged,
}: {
  dep: Deployment;
  onChanged: () => void;
}) {
  const { t } = useTranslation("common");
  const { busy, run } = useOrder();
  const enabled = dep.health.status !== "off";
  const [threshold, setThreshold] = useState<number | null>(
    dep.health.threshold_sec,
  );
  const checkable = healthCheckable(dep.operations);
  const save = async (on: boolean) => {
    const refused = await run(() =>
      api.setDeploymentHealthCheck(dep.id, on, threshold ?? 0),
    );
    if (refused !== null) {
      message.error(refused);
      return;
    }
    message.success(t("healthCheck.saved"));
    onChanged();
  };
  return (
    <Space size={12} wrap>
      <HealthTag health={dep.health} />
      <Switch
        size="small"
        checked={enabled}
        loading={busy}
        // Turning off is always possible; on needs a chat or embedding model.
        disabled={!enabled && !checkable}
        aria-label={t("healthCheck.enable")}
        title={!checkable ? t("healthCheck.unsupported") : undefined}
        onChange={(on) => void save(on)}
      />
      {enabled && (
        <Space size={4}>
          <InputNumber
            size="small"
            aria-label={t("healthCheck.threshold")}
            addonBefore={t("healthCheck.threshold")}
            addonAfter={t("healthCheck.seconds")}
            min={1}
            max={600}
            precision={0}
            value={threshold}
            onChange={setThreshold}
            style={{ width: 170 }}
          />
          <Button
            size="small"
            loading={busy}
            disabled={
              threshold == null || threshold === dep.health.threshold_sec
            }
            onClick={() => void save(true)}
          >
            {t("healthCheck.save")}
          </Button>
        </Space>
      )}
    </Space>
  );
}

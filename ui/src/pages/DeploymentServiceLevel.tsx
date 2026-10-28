import { useState } from "react";
import { Alert, Button, Radio, Space, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, serviceLevels, type Deployment, type ServiceLevel } from "../api";
import { useOrder } from "./useOrder";

// levelOf is the level the server reports; a deployment without one is
// resident.
export function levelOf(dep: Deployment): ServiceLevel {
  return dep.service_level && serviceLevels.includes(dep.service_level) ? dep.service_level : "resident";
}

// canShareGPUs is whether the deployment may leave resident: it has to be able
// to sleep, and to be a single-node, non-PD one (the server refuses the rest).
export function canShareGPUs(dep: Deployment): boolean {
  return dep.sleepable && !dep.pd && (dep.nnodes ?? 1) <= 1;
}

// ServiceLevelEditor sets how the deployment shares its GPUs with the others
// on them. The scheduling policy editor keys it by the saved level, so it
// restarts from what was saved. A level other than resident is refused for a
// deployment that cannot sleep or spans nodes, so those radios are disabled
// (a deployment that kept such a level can still go back to resident).
export default function ServiceLevelEditor({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const { busy, run } = useOrder();
  const saved = levelOf(dep);
  const [level, setLevel] = useState<ServiceLevel>(saved);
  const eligible = canShareGPUs(dep);
  const save = async () => {
    const refused = await run(() => api.setServiceLevel(dep.id, level));
    if (refused !== null) {
      message.error(t("serviceLevel.saveFailed", { detail: refused }));
      return;
    }
    message.success(t("serviceLevel.saved"));
    onChanged();
  };
  return (
    <section aria-label={t("serviceLevel.title")}>
      <Typography.Paragraph type="secondary" style={{ margin: 0, fontSize: 12 }}>{t("serviceLevel.hint")}</Typography.Paragraph>
      {!eligible && <Alert style={{ marginTop: 8 }} type="info" showIcon message={t("serviceLevel.notEligible")} />}
      <Radio.Group
        value={level}
        onChange={(event) => setLevel(event.target.value as ServiceLevel)}
        aria-label={t("serviceLevel.title")}
        style={{ display: "block", marginTop: 8 }}
      >
        <Space direction="vertical" size={8}>
          {serviceLevels.map((value) => (
            <Radio key={value} value={value} disabled={!eligible && value !== "resident"}>
              <strong>{t(`serviceLevel.name.${value}`)}</strong>
              <Typography.Text type="secondary" style={{ display: "block", fontSize: 12 }}>{t(`serviceLevel.description.${value}`)}</Typography.Text>
            </Radio>
          ))}
        </Space>
      </Radio.Group>
      <div style={{ marginTop: 8 }}>
        <Button size="small" loading={busy} disabled={level === saved} onClick={() => void save()}>
          {t("automation.save")}
        </Button>
      </div>
    </section>
  );
}

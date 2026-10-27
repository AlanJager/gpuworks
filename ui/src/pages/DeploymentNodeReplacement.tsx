import { useState } from "react";
import { Button, InputNumber, Space, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type Deployment } from "../api";
import { useOrder } from "./useOrder";

// What a deployment waits for a lost node by default.
const DEFAULT_REPLACE_MINUTES = 10;
// The server accepts 60 s to 7 days.
const MAX_MINUTES = 7 * 24 * 60;

// DeploymentNodeReplacement sets how long a node running one of the
// deployment's replicas may stay offline before the replica moves to another
// node. 0 never moves it.
export default function DeploymentNodeReplacement({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  if (dep.desired_state === "deleting") return null;
  return <NodeReplacementEditor key={`${dep.id}-${dep.node_replacement?.after_sec ?? ""}`} dep={dep} onChanged={onChanged} />;
}

function NodeReplacementEditor({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const { busy, run } = useOrder();
  const savedSec = dep.node_replacement?.after_sec ?? DEFAULT_REPLACE_MINUTES * 60;
  const [minutes, setMinutes] = useState<number | null>(savedSec / 60);
  const save = async () => {
    if (minutes == null) return;
    const refused = await run(() => api.setNodeReplacement(dep.id, { after_sec: Math.round(minutes * 60) }));
    if (refused !== null) {
      message.error(t("nodeReplacement.saveFailed", { detail: refused }));
      return;
    }
    message.success(t("nodeReplacement.saved"));
    onChanged();
  };
  const multi = dep.pd != null || (dep.nnodes ?? 1) > 1;
  return (
    <section aria-label={t("nodeReplacement.title")} style={{ marginTop: 20 }}>
      <Typography.Title level={5} style={{ marginBottom: 0 }}>{t("nodeReplacement.title")}</Typography.Title>
      <Typography.Paragraph type="secondary" style={{ margin: 0, fontSize: 12 }}>
        {t("nodeReplacement.hint")} {multi ? t("nodeReplacement.hintMulti") : t("nodeReplacement.hintSingle")} {t("nodeReplacement.hintRevert")}
      </Typography.Paragraph>
      <Space size={8} wrap style={{ marginTop: 8 }}>
        <InputNumber
          size="small"
          min={0}
          max={MAX_MINUTES}
          value={minutes}
          onChange={setMinutes}
          aria-label={t("nodeReplacement.minutes")}
          addonBefore={t("nodeReplacement.after")}
          addonAfter={t("nodeReplacement.unit")}
          style={{ width: 260 }}
        />
        <Button size="small" loading={busy} disabled={minutes == null || Math.round(minutes * 60) === savedSec} onClick={() => void save()}>
          {t("nodeReplacement.save")}
        </Button>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>{t("nodeReplacement.zeroNever")}</Typography.Text>
      </Space>
    </section>
  );
}

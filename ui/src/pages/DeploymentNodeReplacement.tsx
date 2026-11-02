import { useState } from "react";
import { Button, InputNumber, Space, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type Deployment } from "../api";
import { useOrder } from "./useOrder";

// What a deployment waits for a lost node by default.
export const DEFAULT_REPLACE_MINUTES = 10;
// The server accepts 60 s to 7 days.
const MAX_MINUTES = 7 * 24 * 60;

// nodeReplacementSeconds is how long a node running one of the deployment's
// replicas may stay offline before the replica moves to another node; 0 never
// moves it.
export function nodeReplacementSeconds(dep: Deployment): number {
  return dep.node_replacement?.after_sec ?? DEFAULT_REPLACE_MINUTES * 60;
}

// NodeReplacementEditor sets that wait, in minutes.
export default function NodeReplacementEditor({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const { busy, run } = useOrder();
  const savedSec = nodeReplacementSeconds(dep);
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
  return (
    <section aria-label={t("nodeReplacement.title")}>
      <Space size={8} wrap>
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

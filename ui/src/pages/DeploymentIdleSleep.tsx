import { useState } from "react";
import { Alert, Button, InputNumber, Select, Space, Switch, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type Deployment } from "../api";
import { useOrder } from "./useOrder";

// What a new idle sleep starts from: 15 idle minutes, deep sleep.
const DEFAULT_IDLE_MINUTES = 15;
const DEFAULT_IDLE_LEVEL = 2;
// The server accepts 60 s to 7 days.
const MAX_MINUTES = 7 * 24 * 60;

// IdleSleepEditor sleeps the deployment after it received no user request for
// a while. Off needs nothing; on needs a deployment that can sleep. The
// scheduling policy editor keys it by the saved setting, so it restarts from
// what was saved.
export default function IdleSleepEditor({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const { busy, run } = useOrder();
  const current = dep.idle_sleep;
  const savedOn = (current?.after_sec ?? 0) > 0;
  const [enabled, setEnabled] = useState(savedOn);
  const [minutes, setMinutes] = useState<number | null>(savedOn ? Math.round(current!.after_sec / 60) : DEFAULT_IDLE_MINUTES);
  const [level, setLevel] = useState<1 | 2>(current?.level === 1 ? 1 : DEFAULT_IDLE_LEVEL);
  const sleepable = dep.sleepable;
  const unchanged = enabled === savedOn && (!enabled || (minutes === Math.round((current?.after_sec ?? 0) / 60) && level === (current?.level ?? DEFAULT_IDLE_LEVEL)));
  const save = async () => {
    if (enabled && (minutes == null || minutes < 1)) return;
    const refused = await run(() => api.setIdleSleep(dep.id, { after_sec: enabled ? (minutes ?? 0) * 60 : 0, level }));
    if (refused !== null) {
      message.error(t("automation.idle.saveFailed", { detail: refused }));
      return;
    }
    message.success(t("automation.idle.saved"));
    onChanged();
  };
  return (
    <section aria-label={t("automation.idle.title")}>
      <Typography.Paragraph type="secondary" style={{ margin: 0, fontSize: 12 }}>{t("automation.idle.hint")}</Typography.Paragraph>
      {!sleepable && !savedOn && <Alert style={{ marginTop: 8 }} type="info" showIcon message={t("automation.idle.notSleepable")} />}
      <Space size={12} wrap style={{ marginTop: 8 }}>
        <Switch
          size="small"
          checked={enabled}
          disabled={!sleepable && !enabled}
          aria-label={t("automation.idle.enable")}
          onChange={setEnabled}
        />
        {enabled && (
          <>
            <InputNumber
              size="small"
              min={1}
              max={MAX_MINUTES}
              precision={0}
              value={minutes}
              onChange={setMinutes}
              aria-label={t("automation.idle.minutes")}
              addonBefore={t("automation.idle.after")}
              addonAfter={t("automation.minutes")}
              style={{ width: 200 }}
            />
            <Select<1 | 2>
              size="small"
              value={level}
              onChange={setLevel}
              aria-label={t("automation.idle.level")}
              style={{ width: 200 }}
              options={[
                { value: 1, label: t("automation.level.1") },
                { value: 2, label: t("automation.level.2") },
              ]}
            />
          </>
        )}
        <Button size="small" loading={busy} disabled={unchanged || (enabled && (minutes == null || minutes < 1))} onClick={() => void save()}>
          {t("automation.save")}
        </Button>
      </Space>
    </section>
  );
}

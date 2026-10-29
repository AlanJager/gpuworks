import { useCallback, useEffect, useState } from "react";
import { Button, Card, Descriptions, Popconfirm, Space, Switch, Typography, message } from "antd";
import { ExportOutlined, LoadingOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { api, type ExperienceStatus } from "../../api";
import { apiErrorText } from "../../apiError";

const { Text } = Typography;

export const EXPERIENCE_ANCHOR = "experience";

type Action = "on" | "off" | "reset" | "password";

/**
 * ExperienceCard is the switch for the experience environment: a separate
 * console on its own port with simulated nodes and data. Off, the card is
 * only its title and switch; on, it shows the entry link and the account. A
 * server without one (the environment's own console) answers 404 and the card
 * stays hidden.
 */
export default function ExperienceCard() {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const [status, setStatus] = useState<ExperienceStatus | null>(null);
  const [busy, setBusy] = useState<Action | "">("");

  const load = useCallback(() => {
    api.experience().then(setStatus).catch(() => setStatus(null));
  }, []);
  useEffect(load, [load]);
  // Keep the entry and account current: the environment may have been
  // restarted or its password changed from another tab.
  useEffect(() => {
    if (!status?.enabled) return;
    const id = window.setInterval(load, 10000);
    const onFocus = () => { if (document.visibilityState === "visible") load(); };
    document.addEventListener("visibilitychange", onFocus);
    window.addEventListener("focus", onFocus);
    return () => {
      window.clearInterval(id);
      document.removeEventListener("visibilitychange", onFocus);
      window.removeEventListener("focus", onFocus);
    };
  }, [status?.enabled, load]);
  // While it starts, poll until the console answers.
  useEffect(() => {
    if (!status?.enabled || status.ready) return;
    const id = window.setTimeout(load, 1500);
    return () => window.clearTimeout(id);
  }, [status, load]);

  if (!status) return null;
  const url = `${window.location.protocol}//${window.location.hostname}:${status.port}/`;
  const act = (action: Action) => {
    const call = {
      on: () => api.setExperienceEnabled(true),
      off: () => api.setExperienceEnabled(false),
      reset: api.resetExperience,
      password: api.rotateExperiencePassword,
    }[action];
    setBusy(action);
    call()
      .then(setStatus)
      .catch((err) => message.error(apiErrorText(tCommon, err)))
      .finally(() => setBusy(""));
  };
  const toggle = (
    <Switch
      aria-label={t("experience.title")}
      checked={status.enabled}
      loading={busy === "on" || busy === "off" || (status.enabled && !status.ready)}
      onChange={(on) => act(on ? "on" : "off")}
    />
  );

  return (
    <Card id={EXPERIENCE_ANCHOR} title={t("experience.title")} extra={toggle} styles={status.enabled || status.error ? undefined : { body: { display: "none" } }}>
      {!status.enabled && status.error && <Text type="danger">{t("experience.exited", { error: status.error })}</Text>}
      {status.enabled && !status.ready && (
        <Text type="secondary"><LoadingOutlined style={{ marginRight: 8 }} />{t("experience.starting")}</Text>
      )}
      {status.enabled && status.ready && (
        <Space direction="vertical" size={12} style={{ width: "100%" }}>
          <Descriptions size="small" column={1} colon={false} labelStyle={{ width: 96 }}>
            <Descriptions.Item label={t("experience.address")}>
              <a href={url} target="_blank" rel="noreferrer">{url} <ExportOutlined /></a>
            </Descriptions.Item>
            <Descriptions.Item label={t("experience.user")}><Text copyable>{status.user}</Text></Descriptions.Item>
            <Descriptions.Item label={t("experience.password")}><Text copyable>{status.password}</Text></Descriptions.Item>
          </Descriptions>
          <Text type="secondary">{t("experience.portHint", { port: status.port })}</Text>
          <Space>
            <Popconfirm title={t("experience.resetConfirm")} onConfirm={() => act("reset")}>
              <Button loading={busy === "reset"}>{t("experience.reset")}</Button>
            </Popconfirm>
            <Popconfirm title={t("experience.passwordConfirm")} onConfirm={() => act("password")}>
              <Button loading={busy === "password"}>{t("experience.changePassword")}</Button>
            </Popconfirm>
          </Space>
        </Space>
      )}
    </Card>
  );
}

import { useEffect, useState } from "react";
import { Alert, Button, Card, Form, InputNumber, Space, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { apiErrorText } from "../../apiError";

const { Text } = Typography;

// The server's bounds for deployment.start_timeout_sec
// (store.DeploymentStartTimeoutSec*); it rejects anything outside them.
const minSeconds = 60;
const maxSeconds = 24 * 60 * 60;

// DeploymentPolicyCard edits the cluster's instance start timeout: how long a
// started instance may take to pass its health check when its deployment
// states no timeout of its own. The PUT names only this field; the server
// keeps the rest of the deployment policy as stored.
export default function DeploymentPolicyCard() {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const [seconds, setSeconds] = useState<number | null>(null);
  const [loadError, setLoadError] = useState("");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    api.settings()
      .then((settings) => {
        const value = Number(settings?.deployment?.start_timeout_sec);
        setSeconds(value > 0 ? value : null);
        setLoadError("");
      })
      .catch((error) => setLoadError(apiErrorText(tCommon, error)));
  }, [tCommon]);

  const valid = seconds != null && seconds >= minSeconds && seconds <= maxSeconds;
  const save = async () => {
    if (!valid) return;
    setSaving(true);
    try {
      await api.putSetting("deployment", { start_timeout_sec: seconds });
      message.success(t("deployment.saved"));
    } catch (error) {
      message.error(t("error.save", { detail: apiErrorText(tCommon, error) }));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Card
      title={t("deployment.title")}
      extra={<Button type="primary" loading={saving} disabled={!valid} onClick={save}>{t("common.save")}</Button>}
    >
      <Space direction="vertical" size={4} style={{ width: "100%", maxWidth: 720 }}>
        <Text type="secondary">{t("deployment.description")}</Text>
        {loadError && <Alert type="error" showIcon message={loadError} />}
        <Form layout="vertical" style={{ marginTop: 8 }}>
          <Form.Item
            label={t("deployment.startTimeout")}
            htmlFor="deployment-start-timeout"
            extra={t("deployment.startTimeoutHelp", { min: minSeconds, max: maxSeconds })}
            validateStatus={seconds != null && !valid ? "error" : undefined}
            style={{ marginBottom: 12 }}
          >
            <InputNumber
              id="deployment-start-timeout"
              min={minSeconds}
              max={maxSeconds}
              step={60}
              precision={0}
              value={seconds}
              onChange={(value) => setSeconds(value == null ? null : Number(value))}
              suffix={t("deployment.seconds")}
              style={{ width: 200 }}
            />
          </Form.Item>
        </Form>
      </Space>
    </Card>
  );
}

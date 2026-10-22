import { useEffect, useState } from "react";
import { Space, Switch, Tooltip, Typography, message } from "antd";
import { ExclamationCircleOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { api, type UsageReportStatus } from "../../api";

const { Text } = Typography;

// UsageReportSwitch is the user experience improvement program inside
// Version and updates: a switch, its name and a hint on hover, styled like
// the automatic update check switch beside it.
export default function UsageReportSwitch() {
  const { t } = useTranslation("settings");
  const [status, setStatus] = useState<UsageReportStatus | null>(null);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    let cancelled = false;
    // A server that cannot say leaves the switch out rather than guessing.
    api.usageReport().then((next) => { if (!cancelled) setStatus(next); }).catch(() => {});
    return () => { cancelled = true; };
  }, []);

  if (!status) return null;
  const toggle = async (enabled: boolean) => {
    setSaving(true);
    try {
      setStatus(await api.setUsageReport(enabled));
      void message.success(t("usageReport.saved"));
    } catch (err) {
      void message.error(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };
  const hint = status.editable ? t("usageReport.hint") : `${t("usageReport.hint")} ${t("usageReport.readOnly")}`;
  return (
    <Space align="center">
      <Switch id="usage-report-enabled" checked={status.enabled} loading={saving}
        disabled={!status.editable} onChange={(value) => void toggle(value)} />
      <label htmlFor="usage-report-enabled"><Text>{t("usageReport.title")}</Text></label>
      <Tooltip title={hint}>
        <Text type="secondary"><ExclamationCircleOutlined aria-label={hint} tabIndex={0} style={{ cursor: "help" }} /></Text>
      </Tooltip>
    </Space>
  );
}

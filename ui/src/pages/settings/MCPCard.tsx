import { useCallback, useEffect, useState } from "react";
import { Alert, Button, Card, Collapse, Descriptions, Space, Switch, Typography, message } from "antd";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api, type MCPStatus } from "../../api";
import { apiErrorText } from "../../apiError";
import { useIsAdmin } from "../../components/useIsAdmin";

const { Paragraph, Text } = Typography;

/**
 * MCPCard shows whether the MCP service serves, how an outside agent
 * connects to it with a platform API token, and lets an administrator turn
 * it on or off.
 */
export default function MCPCard() {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const isAdmin = useIsAdmin();
  const [status, setStatus] = useState<MCPStatus>();
  const [loadError, setLoadError] = useState("");
  const [saving, setSaving] = useState(false);

  const load = useCallback(() => api.mcpStatus()
    .then((s) => { setStatus(s); setLoadError(""); })
    .catch((e) => setLoadError(apiErrorText(tCommon, e))), [tCommon]);
  useEffect(() => { void load(); }, [load]);

  const toggle = async (enabled: boolean) => {
    setSaving(true);
    try {
      setStatus(await api.putMCPSettings(enabled));
      message.success(enabled ? t("mcp.turnedOn") : t("mcp.turnedOff"));
    } catch (e) {
      message.error(apiErrorText(tCommon, e));
    } finally {
      setSaving(false);
    }
  };

  const endpoint = `${window.location.origin}${status?.path ?? "/api/v1/mcp"}`;
  const claudeCode = `claude mcp add --transport http gpuworks ${endpoint} \\
  --header "Authorization: Bearer <gwp_token>"`;
  const generic = JSON.stringify({ mcpServers: { gpuworks: { type: "http", url: endpoint, headers: { Authorization: "Bearer <gwp_token>" } } } }, null, 2);

  return (
    <Card
      title={t("mcp.title")}
      extra={
        <Space>
          <Text type="secondary">{status?.enabled ? t("mcp.on") : t("mcp.off")}</Text>
          <Switch
            checked={status?.enabled ?? false}
            loading={saving || (!status && !loadError)}
            disabled={!isAdmin || !status}
            onChange={(checked) => void toggle(checked)}
            aria-label={t("mcp.switch")}
          />
        </Space>
      }
    >
      {loadError && (
        <Alert
          type="error"
          showIcon
          message={loadError}
          action={<Button size="small" onClick={() => void load()}>{tCommon("actions.retry")}</Button>}
          style={{ marginBottom: 12 }}
        />
      )}
      <Paragraph type="secondary">{t("mcp.description")}</Paragraph>
      {!isAdmin && <Paragraph type="secondary">{t("mcp.adminOnly")}</Paragraph>}
      <Descriptions size="small" column={1} bordered style={{ marginBottom: 12 }}>
        <Descriptions.Item label={t("mcp.endpoint")}><Text code copyable>{endpoint}</Text></Descriptions.Item>
        <Descriptions.Item label={t("mcp.auth")}>
          {t("mcp.authText")} <Link to="/apikeys">{t("mcp.createToken")}</Link>
        </Descriptions.Item>
        <Descriptions.Item label={t("mcp.tools")}>
          <Space size={4} wrap>{(status?.tools ?? []).map((name) => <Text code key={name}>{name}</Text>)}</Space>
        </Descriptions.Item>
      </Descriptions>
      <Space size={8} style={{ marginBottom: 4 }}>
        <Text strong>{t("mcp.claudeCode")}</Text>
        <Text copyable={{ text: claudeCode }} />
      </Space>
      <pre className="code-block" style={{ marginBottom: 12 }}>{claudeCode}</pre>
      <Collapse
        ghost
        size="small"
        style={{ marginBottom: 12 }}
        items={[{
          key: "other",
          label: t("mcp.otherClients"),
          // Keyboard activation must not bubble into the collapse header either.
          extra: <Text copyable={{ text: generic }}
            onClick={(e) => e.stopPropagation()} onKeyDown={(e) => e.stopPropagation()} />,
          children: <pre className="code-block">{generic}</pre>,
        }]}
      />
      <Alert type="info" showIcon message={t("mcp.dataNotice")} />
    </Card>
  );
}

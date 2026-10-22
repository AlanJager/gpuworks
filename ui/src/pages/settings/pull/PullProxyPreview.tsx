import { useEffect, useState } from "react";
import { Alert, Space, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { api, type NodePullProxy } from "../../../api";
import NodePicker from "../../../nodes/NodePicker";

/**
 * PullProxyPreview answers "which proxy would this node use": it asks the
 * server which entry the node meets (GET /nodes/{uuid}/pull-proxy). It reads
 * the saved entries only, so unsaved drawer edits are not reflected; when the
 * saved list changes (refreshKey) the answer is asked again.
 */
export default function PullProxyPreview({ refreshKey }: { refreshKey: unknown }) {
  const { t } = useTranslation("settings");
  const [uuid, setUUID] = useState<string | undefined>();
  const [result, setResult] = useState<{ view?: NodePullProxy; error?: string } | null>(null);

  useEffect(() => {
    if (!uuid) return undefined;
    let cancelled = false;
    api.nodePullProxy(uuid)
      .then((view) => { if (!cancelled) setResult({ view }); })
      .catch((err) => { if (!cancelled) setResult({ error: (err as Error).message }); });
    return () => { cancelled = true; };
  }, [uuid, refreshKey]);

  return (
    <Space direction="vertical" size={4} style={{ width: "100%" }}>
      <Typography.Text strong>{t("pull.proxies.previewTitle")}</Typography.Text>
      <NodePicker
        aria-label={t("pull.proxies.previewPick")}
        placeholder={t("pull.proxies.previewPick")}
        allowClear
        style={{ width: 320, maxWidth: "100%" }}
        value={uuid}
        onChange={(value) => {
          setResult(null);
          setUUID(Array.isArray(value) ? value[0] : value);
        }}
      />
      <div aria-live="polite">
        {uuid && result?.error && <Alert type="error" showIcon message={t("pull.proxies.previewFailed", { detail: result.error })} />}
        {uuid && result?.view && (
          <Typography.Text>
            {result.view.proxy ? t("pull.proxies.previewResult", { n: result.view.proxy.position + 1 }) : t("pull.proxies.previewNone")}
          </Typography.Text>
        )}
      </div>
      <Typography.Text type="secondary">{t("pull.proxies.previewHelp")}</Typography.Text>
    </Space>
  );
}

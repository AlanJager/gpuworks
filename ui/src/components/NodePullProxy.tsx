import { Alert, Card, Skeleton, Space, Tag, Typography } from "antd";
import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { api, type NodePullProxy as NodePullProxyView, type NodeRuntime } from "../api";
import { requirementChips } from "../pages/settings/pull/pullFormat";
import NodeRuntimeProxy from "./NodeRuntimeProxy";

/**
 * NodePullProxy shows, read-only, which pull proxy entry this node goes out
 * through (the first entry whose node requirements the node meets; none means
 * the agent keeps its own environment) and the proxy status of the node's
 * container runtime. The entries themselves are edited under Settings, pull
 * settings, proxies.
 */
export default function NodePullProxy({ nodeUUID, statusPollMs }: { nodeUUID: string; statusPollMs?: number }) {
  const { t } = useTranslation("nodes");
  const [loaded, setLoaded] = useState<NodePullProxyView | null>(null);
  const [loadError, setLoadError] = useState<Error | null>(null);

  useEffect(() => {
    let cancelled = false;
    setLoaded(null);
    setLoadError(null);
    api.nodePullProxy(nodeUUID)
      .then((view) => { if (!cancelled) setLoaded(view); })
      .catch((error) => { if (!cancelled) setLoadError(error as Error); });
    return () => { cancelled = true; };
  }, [nodeUUID]);

  const refreshStatus = useCallback(async () => {
    try {
      const fresh = await api.nodePullProxy(nodeUUID);
      setLoaded(fresh);
    } catch {
      // The next poll tries again; the shown status stays.
    }
  }, [nodeUUID]);
  const setRuntime = useCallback((runtime: NodeRuntime) => {
    setLoaded((current) => current && { ...current, runtime });
  }, []);

  if (loadError) return <Alert type="error" showIcon message={t("pullProxy.loadFailed")} description={loadError.message} />;
  if (!loaded) return <Card size="small" title={t("pullProxy.title")}><Skeleton active paragraph={{ rows: 2 }} title={false} /></Card>;

  const entry = loaded.proxy;
  const chips = requirementChips(entry?.node_requirements);
  const direct = t("pullProxy.direct");
  return <Card
    size="small"
    title={<Space>{t("pullProxy.title")}<Tag color={entry ? "blue" : "default"}>{entry ? t("pullProxy.entry", { n: entry.position + 1 }) : t("pullProxy.none")}</Tag></Space>}
    extra={<Link to="/settings#pull-settings">{t("pullProxy.settingsLink")}</Link>}
  >
    <Space direction="vertical" size={8} style={{ width: "100%" }}>
      {entry ? (
        <Space direction="vertical" size={2}>
          <Typography.Text>{t("pullProxy.httpProxy")}: <Typography.Text code>{entry.http_proxy || direct}</Typography.Text></Typography.Text>
          <Typography.Text>{t("pullProxy.httpsProxy")}: <Typography.Text code>{entry.https_proxy || direct}</Typography.Text></Typography.Text>
          {entry.no_proxy && <Typography.Text>NO_PROXY: <Typography.Text code>{entry.no_proxy}</Typography.Text></Typography.Text>}
          {entry.username && <Typography.Text>{t("pullProxy.user", { user: entry.username })}</Typography.Text>}
          <Space size={[4, 4]} wrap>
            <Typography.Text type="secondary">{t("pullProxy.appliesTo")}</Typography.Text>
            {chips.length === 0 ? <Tag>{t("pullProxy.allNodes")}</Tag> : chips.map((chip) => <Tag key={chip}>{chip}</Tag>)}
          </Space>
        </Space>
      ) : (
        <Typography.Text type="secondary">{t("pullProxy.noneDetail")}</Typography.Text>
      )}
      <NodeRuntimeProxy
        nodeUUID={nodeUUID}
        effective={{
          http_proxy: entry?.http_proxy,
          https_proxy: entry?.https_proxy,
          no_proxy: entry?.no_proxy,
        }}
        runtime={loaded.runtime}
        onRuntime={setRuntime}
        onRefresh={refreshStatus}
        statusPollMs={statusPollMs}
      />
    </Space>
  </Card>;
}

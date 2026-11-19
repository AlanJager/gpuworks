import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { api, type NodePullProxy as NodePullProxyView, type NodeRuntime } from "../api";
import { requirementChips } from "../pages/settings/pull/pullFormat";
import NodeRuntimeProxy from "./NodeRuntimeProxy";
import { Badge } from "./ui/badge";
import { Card, CardAction, CardContent, CardHeader, CardTitle } from "./ui/card";
import { Skeleton } from "./ui/skeleton";
import { Notice } from "../ui-kit/Notice";

const CODE = "rounded-sm bg-emphasis px-1 font-mono text-label";

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

  if (loadError) return <Notice tone="danger" title={t("pullProxy.loadFailed")}>{loadError.message}</Notice>;
  if (!loaded) return <Card className="border border-border-subtle" aria-busy="true">
    <CardHeader><CardTitle>{t("pullProxy.title")}</CardTitle></CardHeader>
    <CardContent className="flex flex-col gap-2 pb-4"><Skeleton className="h-4 w-full" /><Skeleton className="h-4 w-3/5" /></CardContent>
  </Card>;

  const entry = loaded.proxy;
  const chips = requirementChips(entry?.node_requirements);
  const direct = t("pullProxy.direct");
  return <Card className="border border-border-subtle">
    <CardHeader>
      <CardTitle><span className="inline-flex items-center gap-2">{t("pullProxy.title")}<Badge size="sm" color={entry ? "blue" : "gray"}>{entry ? t("pullProxy.entry", { n: entry.position + 1 }) : t("pullProxy.none")}</Badge></span></CardTitle>
      <CardAction><Link className="text-fg-brand hover:underline" to="/settings#pull-settings">{t("pullProxy.settingsLink")}</Link></CardAction>
    </CardHeader>
    <CardContent className="flex w-full flex-col gap-2 pb-4">
      {entry ? (
        <div className="flex flex-col gap-0.5">
          <span>{t("pullProxy.httpProxy")}: <code className={CODE}>{entry.http_proxy || direct}</code></span>
          <span>{t("pullProxy.httpsProxy")}: <code className={CODE}>{entry.https_proxy || direct}</code></span>
          {entry.no_proxy && <span>NO_PROXY: <code className={CODE}>{entry.no_proxy}</code></span>}
          {entry.username && <span>{t("pullProxy.user", { user: entry.username })}</span>}
          <span className="inline-flex flex-wrap items-center gap-1">
            <span className="text-fg-muted">{t("pullProxy.appliesTo")}</span>
            {chips.length === 0 ? <Badge size="sm">{t("pullProxy.allNodes")}</Badge> : chips.map((chip) => <Badge size="sm" key={chip}>{chip}</Badge>)}
          </span>
        </div>
      ) : (
        <span className="text-fg-muted">{t("pullProxy.noneDetail")}</span>
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
    </CardContent>
  </Card>;
}

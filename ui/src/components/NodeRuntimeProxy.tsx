import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type NetworkConfig, type NodeRuntime } from "../api";
import { apiErrorText } from "../apiError";
import CopyButton from "./CopyButton";
import { useIsAdmin } from "./useIsAdmin";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "./ui/dialog";
import { message } from "../ui-kit/feedback";
import { Notice } from "../ui-kit/Notice";

const DIALOG_STYLE = { width: "min(520px, calc(100vw - 32px))", maxWidth: "calc(100vw - 32px)" };

/** The systemd drop-in through which the platform hands the proxy to dockerd. */
export const DOCKER_PROXY_DROPIN_PATH = "/etc/systemd/system/docker.service.d/zz-gpuworks-proxy.conf";
export const DOCKER_MANUAL_COMMANDS = ["sudo systemctl daemon-reload", "sudo systemctl restart docker"];
const ALWAYS_NO_PROXY = ["localhost", "127.0.0.1", "::1"];
const DEFAULT_STATUS_POLL_MS = 10000;

function splitList(value?: string): string[] {
  return (value || "").split(",").map((item) => item.trim()).filter(Boolean);
}

/**
 * dropInContent builds the drop-in file from the saved effective settings. It
 * is an approximation of what the agent writes (the agent also adds the server
 * host to NO_PROXY), which the notice says out loud.
 */
export function dropInContent(effective: NetworkConfig): string {
  const lines = ["[Service]"];
  const httpProxy = effective.http_proxy?.trim();
  const httpsProxy = effective.https_proxy?.trim();
  const noProxy = [...new Set([...splitList(effective.no_proxy), ...ALWAYS_NO_PROXY])].join(",");
  if (httpProxy) lines.push(`Environment="HTTP_PROXY=${httpProxy}"`);
  if (httpsProxy) lines.push(`Environment="HTTPS_PROXY=${httpsProxy}"`);
  lines.push(`Environment="NO_PROXY=${noProxy}"`);
  return lines.join("\n");
}

function CodeBlock({ caption, text, copyLabel }: { caption: string; text: string; copyLabel: string }) {
  return <div className="overflow-hidden rounded-lg border border-border">
    <div className="flex items-center justify-between bg-emphasis px-2 py-0.5">
      <code className="font-mono text-label text-fg-muted">{caption}</code>
      <CopyButton text={text} label={copyLabel} size="small" />
    </div>
    <pre tabIndex={0} className="m-0 overflow-x-auto px-3 py-2">{text}</pre>
  </div>;
}

export interface NodeRuntimeProxyProps {
  nodeUUID: string;
  /** What the node would use, from the saved settings. */
  effective: NetworkConfig;
  runtime?: NodeRuntime;
  /** Takes the runtime returned by a restart into the caller's state. */
  onRuntime: (runtime: NodeRuntime) => void;
  /** Re-reads the node's network status; used by the manual-step polling and after a restart. */
  onRefresh: () => Promise<void>;
  statusPollMs?: number;
}

/**
 * NodeRuntimeProxy reports whether the node's container runtime already uses
 * the proxy the platform configured, and offers the way to make it so: a
 * confirmed dockerd restart when the agent may do it, or copyable manual steps
 * when it may not. nerdctl and udocker apply the proxy on every pull and need
 * nothing.
 */
export default function NodeRuntimeProxy({ nodeUUID, effective, runtime, onRuntime, onRefresh, statusPollMs = DEFAULT_STATUS_POLL_MS }: NodeRuntimeProxyProps) {
  const { t } = useTranslation("nodes");
  const { t: tCommon } = useTranslation("common");
  const isAdmin = useIsAdmin();
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [restarting, setRestarting] = useState(false);
  const [restartError, setRestartError] = useState("");
  const statusRef = useRef<HTMLDivElement>(null);

  const driver = runtime?.driver || "";
  const proxy = runtime?.proxy;
  const manualSteps = Boolean(proxy) && !proxy!.applied && !proxy!.daemon_json_proxy && proxy!.systemd && proxy!.privilege === "none";

  // The operator runs the commands on the node by hand; follow the node until
  // it reports the proxy as applied.
  useEffect(() => {
    if (!manualSteps) return undefined;
    const timer = setInterval(() => { void onRefresh(); }, statusPollMs);
    return () => clearInterval(timer);
  }, [manualSteps, statusPollMs, onRefresh]);

  const restart = useCallback(async () => {
    setRestarting(true);
    setRestartError("");
    try {
      const next = await api.restartNodeRuntime(nodeUUID);
      onRuntime(next);
      setConfirmOpen(false);
      message.success(t("runtimeProxy.restarted"));
      void onRefresh();
      setTimeout(() => statusRef.current?.focus(), 0);
    } catch (error) {
      setConfirmOpen(false);
      setRestartError(apiErrorText(tCommon, error));
    } finally {
      setRestarting(false);
    }
  }, [nodeUUID, onRuntime, onRefresh, t, tCommon]);

  const driverLine = <span>{t("runtimeProxy.driver", { driver: driver || t("runtimeProxy.unknownDriver") })}</span>;

  if (!proxy) {
    const note = driver && driver !== "docker" ? t("runtimeProxy.perPull") : t("runtimeProxy.unknownOffline");
    return <div className="flex w-full flex-col gap-1">
      {driverLine}
      <span className="text-fg-muted">{note}</span>
    </div>;
  }

  const direct = t("runtimeProxy.direct");
  const canRestart = isAdmin && !proxy.applied && !proxy.daemon_json_proxy && proxy.systemd && (proxy.privilege === "root" || proxy.privilege === "sudo");
  const privilegeText = proxy.privilege === "root" ? t("runtimeProxy.privilegeRoot")
    : proxy.privilege === "sudo" ? t("runtimeProxy.privilegeSudo")
      : t("runtimeProxy.privilegeNone");

  // With no proxy set for the node the platform leaves dockerd's own proxy
  // alone: it is shown, not judged - unless dockerd still runs a proxy it is
  // no longer configured with (the platform's, just cleared), which takes a
  // restart like any other change.
  const managed = Boolean(effective.http_proxy?.trim() || effective.https_proxy?.trim() || proxy.written_http_proxy || proxy.written_https_proxy || !proxy.applied);
  let status;
  if (!managed) {
    status = <span className="text-fg-muted">{t("runtimeProxy.unmanaged", { proxy: proxy.http_proxy || direct })}</span>;
  } else if (proxy.applied) {
    status = <>
      <Badge status="success">{t("runtimeProxy.applied")}</Badge>
      <span className="text-fg-muted">{t("runtimeProxy.using", { proxy: proxy.written_http_proxy || proxy.http_proxy || direct })}</span>
    </>;
  } else {
    status = <>
      <Badge status="warning">{t("runtimeProxy.notApplied")}</Badge>
      <span>{proxy.daemon_json_proxy ? t("runtimeProxy.daemonJsonWins") : t("runtimeProxy.stillUsing", { proxy: proxy.http_proxy || direct })}</span>
    </>;
  }

  return <div className="flex w-full flex-col gap-2">
    {driverLine}
    <div ref={statusRef} tabIndex={-1} aria-live="polite" className="outline-none">
      <span className="inline-flex flex-wrap items-center gap-1">{status}</span>
    </div>
    <span className="text-fg-muted">
      {[
        proxy.live_restore ? t("runtimeProxy.liveRestoreOn") : t("runtimeProxy.liveRestoreOff"),
        proxy.systemd ? t("runtimeProxy.systemd") : t("runtimeProxy.noSystemd"),
        privilegeText,
      ].join(" · ")}
    </span>
    {proxy.error ? <Notice tone="danger" title={proxy.error} /> : null}
    {restartError ? <Notice tone="danger" role="alert" title={t("runtimeProxy.restartFailed")}>{restartError}</Notice> : null}

    {canRestart ? <div>
      <Button variant="tertiary" onClick={() => { setRestartError(""); setConfirmOpen(true); }}>{t("runtimeProxy.restart")}</Button>
    </div> : null}

    {!proxy.applied && !proxy.daemon_json_proxy && !proxy.systemd ? <Notice tone="info" title={t("runtimeProxy.noSystemdNote")} /> : null}

    {manualSteps ? <Notice tone="warning" title={t("runtimeProxy.noPrivilege")}>
      <div className="flex w-full flex-col gap-2">
        <span className="text-fg-muted">{t("runtimeProxy.fileHint")}</span>
        <CodeBlock caption={DOCKER_PROXY_DROPIN_PATH} text={dropInContent(effective)} copyLabel={t("runtimeProxy.copyFile")} />
        <span className="text-fg-muted">{t("runtimeProxy.fileAuthority")}</span>
        {DOCKER_MANUAL_COMMANDS.map((command) => <CodeBlock key={command} caption="shell" text={command} copyLabel={t("runtimeProxy.copyCommand", { command })} />)}
        <span className="inline-flex items-center gap-2">
          <span className="text-fg-muted">{t("runtimeProxy.autoRefresh")}</span>
          <Button variant="tertiary" size="sm" onClick={() => void onRefresh()}>{t("runtimeProxy.refresh")}</Button>
        </span>
      </div>
    </Notice> : null}

    <Dialog open={confirmOpen} disablePointerDismissal onOpenChange={(next) => { if (!next && !restarting) setConfirmOpen(false); }}>
      <DialogContent closeButtonLabel={tCommon("actions.close")} closeButtonDisabled={restarting} style={DIALOG_STYLE}>
        <DialogHeader>
          <DialogTitle>{t("runtimeProxy.confirmTitle")}</DialogTitle>
          <DialogDescription>{proxy.live_restore ? t("runtimeProxy.confirmLive") : t("runtimeProxy.confirmNoLive")}</DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="ghost" autoFocus disabled={restarting} onClick={() => setConfirmOpen(false)}>{t("action.cancel")}</Button>
          <Button variant={proxy.live_restore ? "primary" : "destructive"} loading={restarting} onClick={() => void restart()}>{t("runtimeProxy.confirmOk")}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  </div>;
}

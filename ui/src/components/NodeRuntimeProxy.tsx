import { WarningOutlined } from "@ant-design/icons";
import { Alert, Button, Modal, Space, Tag, Typography, message } from "antd";
import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type NetworkConfig, type NodeRuntime } from "../api";
import { apiErrorText } from "../apiError";
import CopyButton from "./CopyButton";
import { useIsAdmin } from "./useIsAdmin";

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
  return <div style={{ border: "1px solid var(--ant-color-border, #d9d9d9)", borderRadius: 6, overflow: "hidden" }}>
    <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", padding: "2px 8px", background: "var(--ant-color-fill-quaternary, #fafafa)" }}>
      <Typography.Text type="secondary" code>{caption}</Typography.Text>
      <CopyButton text={text} label={copyLabel} size="small" />
    </div>
    <pre tabIndex={0} style={{ margin: 0, padding: "8px 12px", overflowX: "auto" }}>{text}</pre>
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

  const driverLine = <Typography.Text>{t("runtimeProxy.driver", { driver: driver || t("runtimeProxy.unknownDriver") })}</Typography.Text>;

  if (!proxy) {
    const note = driver && driver !== "docker" ? t("runtimeProxy.perPull") : t("runtimeProxy.unknownOffline");
    return <Space direction="vertical" size={4} style={{ width: "100%" }}>
      {driverLine}
      <Typography.Text type="secondary">{note}</Typography.Text>
    </Space>;
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
    status = <Typography.Text type="secondary">{t("runtimeProxy.unmanaged", { proxy: proxy.http_proxy || direct })}</Typography.Text>;
  } else if (proxy.applied) {
    status = <>
      <Tag color="green">{t("runtimeProxy.applied")}</Tag>
      <Typography.Text type="secondary">{t("runtimeProxy.using", { proxy: proxy.written_http_proxy || proxy.http_proxy || direct })}</Typography.Text>
    </>;
  } else {
    status = <>
      <Tag color="orange" icon={<WarningOutlined />}>{t("runtimeProxy.notApplied")}</Tag>
      <Typography.Text>{proxy.daemon_json_proxy ? t("runtimeProxy.daemonJsonWins") : t("runtimeProxy.stillUsing", { proxy: proxy.http_proxy || direct })}</Typography.Text>
    </>;
  }

  return <Space direction="vertical" size={8} style={{ width: "100%" }}>
    {driverLine}
    <div ref={statusRef} tabIndex={-1} aria-live="polite" style={{ outline: "none" }}>
      <Space wrap size={4}>{status}</Space>
    </div>
    <Typography.Text type="secondary">
      {[
        proxy.live_restore ? t("runtimeProxy.liveRestoreOn") : t("runtimeProxy.liveRestoreOff"),
        proxy.systemd ? t("runtimeProxy.systemd") : t("runtimeProxy.noSystemd"),
        privilegeText,
      ].join(" · ")}
    </Typography.Text>
    {proxy.error ? <Alert type="error" showIcon message={proxy.error} /> : null}
    {restartError ? <Alert type="error" showIcon role="alert" message={t("runtimeProxy.restartFailed")} description={restartError} /> : null}

    {canRestart ? <div>
      <Button onClick={() => { setRestartError(""); setConfirmOpen(true); }}>{t("runtimeProxy.restart")}</Button>
    </div> : null}

    {!proxy.applied && !proxy.daemon_json_proxy && !proxy.systemd ? <Alert type="info" showIcon message={t("runtimeProxy.noSystemdNote")} /> : null}

    {manualSteps ? <Alert
      type="warning"
      showIcon
      message={t("runtimeProxy.noPrivilege")}
      description={<Space direction="vertical" size={8} style={{ width: "100%" }}>
        <Typography.Text type="secondary">{t("runtimeProxy.fileHint")}</Typography.Text>
        <CodeBlock caption={DOCKER_PROXY_DROPIN_PATH} text={dropInContent(effective)} copyLabel={t("runtimeProxy.copyFile")} />
        <Typography.Text type="secondary">{t("runtimeProxy.fileAuthority")}</Typography.Text>
        {DOCKER_MANUAL_COMMANDS.map((command) => <CodeBlock key={command} caption="shell" text={command} copyLabel={t("runtimeProxy.copyCommand", { command })} />)}
        <Space>
          <Typography.Text type="secondary">{t("runtimeProxy.autoRefresh")}</Typography.Text>
          <Button size="small" onClick={() => void onRefresh()}>{t("runtimeProxy.refresh")}</Button>
        </Space>
      </Space>}
    /> : null}

    <Modal
      open={confirmOpen}
      title={t("runtimeProxy.confirmTitle")}
      okText={t("runtimeProxy.confirmOk")}
      cancelText={t("action.cancel")}
      okButtonProps={{ danger: !proxy.live_restore }}
      cancelButtonProps={{ autoFocus: true, disabled: restarting }}
      confirmLoading={restarting}
      closable={!restarting}
      maskClosable={false}
      keyboard={!restarting}
      onOk={() => void restart()}
      onCancel={() => { if (!restarting) setConfirmOpen(false); }}
    >
      <Typography.Paragraph>{proxy.live_restore ? t("runtimeProxy.confirmLive") : t("runtimeProxy.confirmNoLive")}</Typography.Paragraph>
    </Modal>
  </Space>;
}

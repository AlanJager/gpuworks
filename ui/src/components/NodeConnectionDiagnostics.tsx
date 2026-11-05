import { Alert, Button, Card, Descriptions, Space, Typography } from "antd";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { formatTime } from "../i18n/datetime";
import { nodeConnectionApi, type NodeConnectionDiagnostics as Report } from "../nodeConnectionApi";
import { recoveryLabel } from "../nodes/recoveryPhase";
import { useIsAdmin } from "./useIsAdmin";

const outcomes = new Set(["unsupported", "credentials_unavailable", "credential_error", "unavailable", "not_checked", "not_applicable", "invalid_address", "unverified_identity", "ssh_failed", "cancelled", "timeout", "connected", "command_failed", "invalid_response", "snapshot", "tool_missing", "http_error", "http_ok", "dns", "connect", "tls", "transport"]);
const exposureStates = new Set(["ready", "starting", "awaiting_route", "degraded", "disabled", "failed"]);
const guidance = new Set(["snapshot_scope", "http_scope", "test_from_node", "manual_address_update", "addresses_differ", "configuration_differs", "server_dial_scope"]);

export default function NodeConnectionDiagnostics({ nodeUUID }: { nodeUUID: string }) {
  const { t } = useTranslation("nodes");
  const admin = useIsAdmin();
  const [report, setReport] = useState<Report | null>(null);
  const [error, setError] = useState(false);
  const [loading, setLoading] = useState(false);
  const active = useRef<AbortController | null>(null);
  const generation = useRef(0);

  useEffect(() => {
    generation.current++;
    active.current?.abort();
    setReport(null);
    setError(false);
    setLoading(false);
    return () => { generation.current++; active.current?.abort(); };
  }, [nodeUUID]);

  const run = async () => {
    const current = ++generation.current;
    active.current?.abort();
    const controller = new AbortController();
    active.current = controller;
    setLoading(true);
    setError(false);
    setReport(null);
    try {
      const next = await nodeConnectionApi.diagnose(nodeUUID, controller.signal);
      if (current === generation.current) setReport(next);
    } catch {
      if (current === generation.current) setError(true);
    } finally {
      if (current === generation.current) setLoading(false);
    }
  };

  if (!admin) return null;
  return <Card size="small" title={t("connectionDiagnostics.title")} extra={<Button aria-label={t("connectionDiagnostics.run")} loading={loading} onClick={run}>{t("connectionDiagnostics.run")}</Button>}>
    <Space direction="vertical" size={12} style={{ width: "100%" }}>
      <Typography.Text type="secondary">{t("connectionDiagnostics.scope")}</Typography.Text>
      {!report && !loading && !error && <Typography.Text>{t("connectionDiagnostics.empty")}</Typography.Text>}
      {loading && <Typography.Text role="status">{t("connectionDiagnostics.loading")}</Typography.Text>}
      {error && <Alert type="error" showIcon message={t("connectionDiagnostics.failed")} />}
      {report && <>
        <Descriptions column={1} size="small" bordered>
          <Descriptions.Item label={t("connectionDiagnostics.checkedAt")}>{formatTime(report.checked_at)}</Descriptions.Item>
          <Descriptions.Item label={t("connectionDiagnostics.savedAddress")}>{report.saved_server_url || t("connectionDiagnostics.unknown")}</Descriptions.Item>
          <Descriptions.Item label={t("connectionDiagnostics.lastSeen")}>{report.last_seen_at ? formatTime(report.last_seen_at) : t("connectionDiagnostics.unknown")}</Descriptions.Item>
          <Descriptions.Item label={t("connectionDiagnostics.recoveryPhase")}>{recoveryLabel[report.recovery_phase] ? t(recoveryLabel[report.recovery_phase].key) : t("connectionDiagnostics.unknown")}</Descriptions.Item>
          <Descriptions.Item label={t("connectionDiagnostics.lastRecovery")}>{report.last_probe_at ? formatTime(report.last_probe_at) : t("connectionDiagnostics.unknown")} · {t("connectionDiagnostics.attempts", { count: report.recovery_attempts })}</Descriptions.Item>
          {report.last_recovery_error && <Descriptions.Item label={t("connectionDiagnostics.recoveryError")}>{report.last_recovery_error}</Descriptions.Item>}
          <Descriptions.Item label={t("connectionDiagnostics.exposure")}>
            {report.exposure ? <Space direction="vertical" size={0}><span>{report.exposure.url}</span><span>{t(`connectionDiagnostics.exposureState.${exposureStates.has(report.exposure.state) ? report.exposure.state : "unknown"}`)}</span><Typography.Text type="secondary">{t("connectionDiagnostics.lastObserved", { time: formatTime(report.exposure.observed_at) })}</Typography.Text></Space> : t("connectionDiagnostics.unknown")}
          </Descriptions.Item>
        </Descriptions>
        {report.checks.map((check) => <Descriptions key={check.stage} column={1} size="small" bordered title={t(`connectionDiagnostics.stage.${check.stage}`)}>
          <Descriptions.Item label={t("connectionDiagnostics.result")}>{t(`connectionDiagnostics.outcome.${outcomes.has(check.outcome) ? check.outcome : "unknown"}`)}{check.http_status ? ` · HTTP ${check.http_status}` : ""}</Descriptions.Item>
          <Descriptions.Item label={t("connectionDiagnostics.vantage")}>{t(`connectionDiagnostics.location.${check.vantage}`)}</Descriptions.Item>
          <Descriptions.Item label={t("connectionDiagnostics.target")}>{check.target || t("connectionDiagnostics.unknown")}</Descriptions.Item>
          {check.source && <Descriptions.Item label={t("connectionDiagnostics.source")}>{t(`connectionDiagnostics.sourceName.${check.source}`)}</Descriptions.Item>}
          <Descriptions.Item label={t("connectionDiagnostics.observedAt")}>{check.observed_at ? formatTime(check.observed_at) : t("connectionDiagnostics.unknown")}</Descriptions.Item>
        </Descriptions>)}
        {report.guidance.filter((code) => guidance.has(code)).map((code) => <Alert key={code} type="info" showIcon message={t(`connectionDiagnostics.guidance.${code}`)} />)}
      </>}
    </Space>
  </Card>;
}

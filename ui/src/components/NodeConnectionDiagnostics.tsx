import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import type { NodeDoctorReport } from "../api";
import { formatTime } from "../i18n/datetime";
import { nodeConnectionApi, type NodeConnectionCheck, type NodeConnectionDiagnostics as Report } from "../nodeConnectionApi";
import { recoveryLabel } from "../nodes/recoveryPhase";
import { recoveryErrorText } from "../nodes/recoveryError";
import { useIsAdmin } from "./useIsAdmin";
import { Button } from "./ui/button";
import { Card, CardAction, CardContent, CardHeader, CardTitle } from "./ui/card";
import { DetailList, DetailListItem, DetailListLabel, DetailListSection, DetailListSectionLabel, DetailListValue } from "./ui/detail-list";
import { Notice } from "../ui-kit/Notice";

const outcomes = new Set(["runtime_snapshot", "offline", "unsupported", "credentials_unavailable", "credential_error", "unavailable", "not_checked", "not_applicable", "invalid_address", "unverified_identity", "ssh_failed", "cancelled", "timeout", "connected", "command_failed", "invalid_response", "snapshot", "tool_missing", "http_error", "http_ok", "dns", "connect", "tls", "transport"]);
const exposureStates = new Set(["ready", "starting", "awaiting_route", "degraded", "disabled", "failed"]);
const guidance = new Set(["snapshot_scope", "http_scope", "test_from_node", "manual_address_update", "addresses_differ", "configuration_differs", "server_dial_scope"]);
const checkStages = new Set(["agent_control", "ssh", "configuration", "node_http"]);
const reasons = new Set(["bad_token", "proto_mismatch", "stale_session", "dns", "connect", "tls", "ws_upgrade", "timeout", "cancelled", "transport", "invalid_configuration", "none"]);
const rejectReasons = new Set(["bad_token", "proto_mismatch", "stale_session"]);
const runtimeFields = ["source", "checked_at", "server_url", "connect_mode", "server_source", "token_present", "control", "data_lane", "outcome", "reason", "retry_at", "proxy", "proxy_url"] as const;
const runtimeEnums: Record<string, ReadonlySet<string>> = {
  source: new Set(["agent_runtime"]),
  connect_mode: new Set(["agent_dial", "server_dial"]),
  server_source: new Set(["flag", "environment", "state_file", "missing", "agent_runtime", "unknown"]),
  token_present: new Set(["true", "false"]),
  control: new Set(["probing", "connected", "not_connected", "smoke_verified", "not_applicable", "unknown"]),
  data_lane: new Set(["unverified", "attached", "detached", "disabled", "not_negotiated", "pending", "not_applicable", "unknown"]),
  outcome: new Set(["ok", "failed", "invalid_configuration", "not_applicable", "probing", "unknown"]),
  reason: reasons,
  proxy: new Set(["direct", "environment_proxy", "unknown"]),
};
const runtimeStages = ["configuration", "dial", "dns", "connect", "proxy_connect", "tls", "ws_upgrade", "yamux", "hello", "wait_registered", "server_hello", "session"] as const;
const phases = new Set(["ok", "failed", "timeout", "cancelled", "in_progress", "not_observed", "not_checked", "not_applicable"]);

function fact(report: NodeDoctorReport, name: string): string {
  const matches = Array.isArray(report.facts) ? report.facts.filter((item) => item?.code === `connection.${name}`) : [];
  const value = matches.length === 1 ? matches[0].value : "";
  return typeof value === "string" && value.length <= 2048 && !/[\u0000-\u001f\u007f-\u009f]/u.test(value) ? value : "";
}

function displayURL(value: string, originOnly = false): string {
  try {
    const url = new URL(value);
    if (!["http:", "https:"].includes(url.protocol) || url.username || url.password || url.search || url.hash || /[\u0000-\u0020\u007f-\u009f]/u.test(value)) return "";
    if (originOnly && value !== url.origin) return "";
    return value;
  } catch { return ""; }
}

function displayTime(value: string): string {
  return /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/u.test(value) && Number.isFinite(Date.parse(value)) ? formatTime(value) : "";
}

function Fact({ label, children }: { label: ReactNode; children: ReactNode }) {
  return <DetailListItem><DetailListLabel>{label}</DetailListLabel><DetailListValue>{children}</DetailListValue></DetailListItem>;
}

/** A titled group of facts; the title names the group for assistive technology. */
function FactGroup({ title, children }: { title: ReactNode; children: ReactNode }) {
  const id = useId();
  return <DetailList><DetailListSection aria-labelledby={id}><DetailListSectionLabel id={id}>{title}</DetailListSectionLabel>{children}</DetailListSection></DetailList>;
}

function SectionTitle({ children }: { children: ReactNode }) {
  return <h3 className="mb-2 text-body font-semibold">{children}</h3>;
}

function RuntimeSnapshot({ report }: { report: NodeDoctorReport }) {
  const { t } = useTranslation("nodes");
  if (fact(report, "source") !== "agent_runtime" || !displayTime(fact(report, "checked_at"))) return null;
  const unknown = t("connectionDiagnostics.unknown");
  const text = (name: typeof runtimeFields[number]) => {
    const value = fact(report, name);
    if (runtimeEnums[name]) return runtimeEnums[name].has(value) ? t(`connectionDiagnostics.runtime.value.${name}.${value}`) : unknown;
    if (name === "checked_at" || name === "retry_at") return displayTime(value) || unknown;
    return displayURL(value, name === "proxy_url") || unknown;
  };
  return <>
    <DetailList>
      {runtimeFields.map((name) => <Fact key={name} label={t(`connectionDiagnostics.runtime.field.${name}`)}>{text(name)}</Fact>)}
    </DetailList>
    {runtimeStages.map((stage) => {
      const phase = fact(report, `phase.${stage}`);
      const elapsed = fact(report, `elapsed_ms.${stage}`);
      const observed = fact(report, `observed_at.${stage}`);
      if (!phase && !elapsed && !observed) return null;
      return <FactGroup key={stage} title={t(`connectionDiagnostics.runtime.stage.${stage}`)}>
        <Fact label={t("connectionDiagnostics.result")}>{phases.has(phase) ? t(`connectionDiagnostics.runtime.phase.${phase}`) : unknown}</Fact>
        <Fact label={t("connectionDiagnostics.runtime.elapsed")}>{/^\d+$/u.test(elapsed) && Number(elapsed) <= 86400000 ? t("connectionDiagnostics.runtime.milliseconds", { count: Number(elapsed) }) : unknown}</Fact>
        <Fact label={t("connectionDiagnostics.observedAt")}>{displayTime(observed) || unknown}</Fact>
      </FactGroup>;
    })}
  </>;
}

function ConnectionCheck({ check }: { check: NodeConnectionCheck }) {
  const { t } = useTranslation("nodes");
  if (!checkStages.has(check.stage)) return null;
  const location = ["control_plane", "node"].includes(check.vantage) ? check.vantage : "unknown";
  const source = check.source && ["state_file", "legacy_arguments"].includes(check.source) ? check.source : "";
  const status = Number.isInteger(check.http_status) && check.http_status! >= 100 && check.http_status! <= 599 ? check.http_status : undefined;
  return <FactGroup title={t(`connectionDiagnostics.stage.${check.stage}`)}>
    <Fact label={t("connectionDiagnostics.result")}>{t(`connectionDiagnostics.outcome.${outcomes.has(check.outcome) ? check.outcome : "unknown"}`)}{status ? ` · HTTP ${status}` : ""}</Fact>
    <Fact label={t("connectionDiagnostics.vantage")}>{t(`connectionDiagnostics.location.${location}`)}</Fact>
    {check.stage !== "agent_control" && <Fact label={t("connectionDiagnostics.target")}>{displayURL(check.target ?? "") || t("connectionDiagnostics.unknown")}</Fact>}
    {source && <Fact label={t("connectionDiagnostics.source")}>{t(`connectionDiagnostics.sourceName.${source}`)}</Fact>}
    <Fact label={t("connectionDiagnostics.observedAt")}>{displayTime(check.observed_at ?? "") || t("connectionDiagnostics.unknown")}</Fact>
  </FactGroup>;
}

export default function NodeConnectionDiagnostics({ nodeUUID }: { nodeUUID: string }) {
  const { t } = useTranslation("nodes");
  const admin = useIsAdmin();
  const [report, setReport] = useState<Report | null>(null);
  const [error, setError] = useState(false);
  const [loading, setLoading] = useState(false);
  const active = useRef<AbortController | null>(null);
  const generation = useRef(0);
  const currentNode = useRef(nodeUUID);
  currentNode.current = nodeUUID;

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
      if (current === generation.current && currentNode.current === nodeUUID) {
        if (next.node_uuid !== nodeUUID) throw new Error("mismatched diagnostic node");
        setReport(next);
      }
    } catch {
      if (current === generation.current && currentNode.current === nodeUUID) setError(true);
    } finally {
      if (current === generation.current && currentNode.current === nodeUUID) setLoading(false);
    }
  };

  if (!admin) return null;
  return <Card className="border border-border-subtle">
    <CardHeader>
      <CardTitle>{t("connectionDiagnostics.title")}</CardTitle>
      <CardAction><Button variant="tertiary" size="sm" aria-label={t("connectionDiagnostics.run")} loading={loading} onClick={run}>{t("connectionDiagnostics.run")}</Button></CardAction>
    </CardHeader>
    <CardContent className="flex w-full flex-col gap-3 pb-4 [overflow-wrap:anywhere]">
      <span className="text-fg-muted">{t("connectionDiagnostics.scope")}</span>
      {!report && !loading && !error && <span>{t("connectionDiagnostics.empty")}</span>}
      {loading && <span role="status">{t("connectionDiagnostics.loading")}</span>}
      {error && <Notice tone="danger" title={t("connectionDiagnostics.failed")} />}
      {report && <>
        <section aria-label={t("connectionDiagnostics.currentTitle")} className="w-full">
          <SectionTitle>{t("connectionDiagnostics.currentTitle")}</SectionTitle>
          <div className="flex w-full flex-col gap-3">
            <DetailList><Fact label={t("connectionDiagnostics.checkedAt")}>{displayTime(report.checked_at) || t("connectionDiagnostics.unknown")}</Fact></DetailList>
            {report.checks.filter((check) => check.stage === "agent_control").map((check) => <ConnectionCheck key={check.stage} check={check} />)}
            {report.agent_report && <RuntimeSnapshot report={report.agent_report} />}
            <span className="text-fg-muted">{t("connectionDiagnostics.runtime.scope")}</span>
          </div>
        </section>
        <section aria-label={t("connectionDiagnostics.historyTitle")} className="w-full">
          <SectionTitle>{t("connectionDiagnostics.historyTitle")}</SectionTitle>
          <DetailList>
            <Fact label={t("connectionDiagnostics.savedAddress")}>{displayURL(report.saved_server_url) || t("connectionDiagnostics.unknown")}</Fact>
            <Fact label={t("connectionDiagnostics.lastSeen")}>{report.last_seen_at ? formatTime(report.last_seen_at) : t("connectionDiagnostics.unknown")}</Fact>
            <Fact label={t("connectionDiagnostics.recoveryPhase")}>{recoveryLabel[report.recovery_phase] ? t(recoveryLabel[report.recovery_phase].key) : t("connectionDiagnostics.unknown")}</Fact>
            <Fact label={t("connectionDiagnostics.lastRecovery")}>{report.last_probe_at ? formatTime(report.last_probe_at) : t("connectionDiagnostics.unknown")} · {t("connectionDiagnostics.attempts", { count: report.recovery_attempts })}</Fact>
            {report.last_recovery_error && <Fact label={t("connectionDiagnostics.recoveryError")}><span className="whitespace-pre-line">{recoveryErrorText(t, report.last_recovery_code, report.last_recovery_error)}</span></Fact>}
            {report.last_reject_reason && rejectReasons.has(report.last_reject_reason) && <Fact label={t("connectionDiagnostics.lastReject")}>{t(`connectionDiagnostics.runtime.value.reason.${report.last_reject_reason}`)}{report.last_reject_at ? ` · ${displayTime(report.last_reject_at)}` : ""}</Fact>}
            <Fact label={t("connectionDiagnostics.exposure")}>
              {report.exposure ? <span className="flex flex-col"><span>{displayURL(report.exposure.url) || t("connectionDiagnostics.unknown")}</span><span>{t(`connectionDiagnostics.exposureState.${exposureStates.has(report.exposure.state) ? report.exposure.state : "unknown"}`)}</span><span className="text-fg-muted">{t("connectionDiagnostics.lastObserved", { time: displayTime(report.exposure.observed_at) || t("connectionDiagnostics.unknown") })}</span></span> : t("connectionDiagnostics.unknown")}
            </Fact>
          </DetailList>
        </section>
        <section aria-label={t("connectionDiagnostics.sshTitle")} className="w-full">
          <SectionTitle>{t("connectionDiagnostics.sshTitle")}</SectionTitle>
          <div className="flex w-full flex-col gap-3">{report.checks.filter((check) => check.stage !== "agent_control").map((check) => <ConnectionCheck key={check.stage} check={check} />)}</div>
        </section>
        <section aria-label={t("connectionDiagnostics.cliTitle")} className="w-full">
          <SectionTitle>{t("connectionDiagnostics.cliTitle")}</SectionTitle>
          <p className="mb-2">{t("connectionDiagnostics.cliScope")}</p>
          <p className="m-0"><code className="whitespace-pre-wrap rounded-sm bg-emphasis px-1 font-mono text-label [overflow-wrap:anywhere]">{t("connectionDiagnostics.cliCommand")}</code></p>
        </section>
        {report.guidance.filter((code) => guidance.has(code)).map((code) => <Notice key={code} tone="info" title={t(`connectionDiagnostics.guidance.${code}`)} />)}
      </>}
    </CardContent>
  </Card>;
}

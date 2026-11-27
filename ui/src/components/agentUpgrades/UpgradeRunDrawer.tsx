import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, useEvents, usePolling } from "../../api";
import { agentUpgradeRunning, type AgentUpgradeItem, type AgentUpgradeRun } from "../../agentUpgradesTypes";
import { formatTimeSec } from "../../i18n/datetime";
import { shortBuild } from "../../buildId";
import { useAgentUpgradeAction } from "../../agentUpgrades";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import { ConfirmPopover } from "../../ui-kit/ConfirmPopover";
import { HelpTip } from "../../ui-kit/HelpTip";
import { Notice } from "../../ui-kit/Notice";
import { OptionSelect } from "../../ui-kit/OptionSelect";
import { Pager } from "../../ui-kit/Pager";
import { SideDrawer } from "../../ui-kit/SideDrawer";
import { Spinner } from "../../ui-kit/Spinner";
import UpgradeSummary from "./UpgradeSummary";

const PAGE_SIZE = 10;

/** Column widths; the table is their sum wide, so a wide drawer shows no scroll and a narrow one scrolls rather than squeezing a column. */
const COLUMN_WIDTHS = { node: 220, from: 150, current: 150, phase: 140, reason: 280, time: 250 };
const TABLE_WIDTH = Object.values(COLUMN_WIDTHS).reduce((sum, width) => sum + width, 0);

const phases = ["queued", "transferring", "waiting_registration", "observing_recovery", "succeeded", "failed", "skipped", "cancelled", "interrupted", "unconfirmed"];
/** The filter's value for every result; the API reads no state filter as all. */
const ALL = "all";

/** Verification codes whose note is the system's fixed English line, said by the code's own text instead; every other note (an operator's, an agent's failure detail) is shown. */
const systemNoteCodes = new Set(["target_registered", "node_removed", "reconnected_old_version"]);

/** One line for an attempt's times: the finish drops the date when it is the start's day. */
function timeRange(started?: string, finished?: string): string {
  const start = formatTimeSec(started), end = formatTimeSec(finished);
  if (!started) return end;
  if (!finished) return `${start} → —`;
  return `${start} → ${end.slice(0, 10) === start.slice(0, 10) ? end.slice(11) : end}`;
}

export default function UpgradeRunDrawer({ runID, isAdmin, onClose, onContinue, onChanged }: {
  runID: number;
  isAdmin: boolean;
  onClose: () => void;
  onContinue: (id: number) => void;
  onChanged: () => void;
}) {
  const { t, i18n } = useTranslation("nodes");
  const [watch, setWatch] = useState(true);
  const interval = watch ? 5000 : 0;
  const status = usePolling(() => api.agentUpgradeRun(runID), interval, [runID, interval]);
  const run: AgentUpgradeRun | null = status.data;
  useEffect(() => { if (run) setWatch(agentUpgradeRunning(run) || run.blocked_by_item?.run_id === run.id); }, [run]);
  const [page, setPage] = useState(1);
  const [phase, setPhase] = useState(ALL);
  const filter = phase === ALL ? "" : phase;
  // The page the rows on screen belong to. A new page or filter keeps the old
  // rows until its answer arrives (usePolling keeps data across a dependency
  // change); until then the table says it is loading. Polls of the same page
  // do not, so the table does not flicker every five seconds.
  const wanted = `${page}\u0000${filter}`;
  const [shownFor, setShownFor] = useState(wanted);
  const items = usePolling(async () => {
    const answer = await api.agentUpgradeItems(runID, page, PAGE_SIZE, filter);
    setShownFor(wanted);
    return answer;
  }, interval, [runID, page, filter, interval]);
  // A page that failed to load is not still loading: the error notice above
  // says what happened, and the cover lifts so the two do not contradict.
  const switching = shownFor !== wanted && !items.error;
  const refresh = () => { void status.refresh(); void items.refresh(); onChanged(); };
  useEvents("agent_upgrade", refresh);
  const [busy, setBusy] = useState(false);
  const perform = useAgentUpgradeAction();
  const [error, setError] = useState("");
  const stop = () => perform(async () => {
    if (busy) return;
    setBusy(true); setError("");
    try { await api.stopAgentUpgrade(runID); refresh(); }
    catch (e) { setError((e as Error).message); }
    finally { setBusy(false); }
  });
  const version = (value?: string) => <span className="whitespace-nowrap" title={value}>{shortBuild(value) || "—"}</span>;
  const rows: AgentUpgradeItem[] = items.data?.items ?? [];
  const failure = error || status.error?.message || items.error?.message;
  const canStop = !!run && (run.counts.queued > 0 || run.counts.active > 0) && !run.stop_requested_at;
  const canContinue = !!run && run.counts.failed + run.counts.interrupted + run.counts.cancelled + run.counts.queued + run.counts.unconfirmed > 0 && (run.state === "paused" || !agentUpgradeRunning(run));
  const continueBlocked = !!run?.blocked_by_item && !(["paused", "interrupted"].includes(run.state) && run.counts.active === 0 && run.blocked_by_item.run_id === run.id && run.blocked_by_item.reason === "active_run");
  return <SideDrawer open title={t("batchUpgrade.runTitle", { id: runID })} width={1280} onClose={onClose}>
    <div className="flex flex-col gap-4">
      {status.loading && !run && <Spinner label={t("batchUpgrade.loadingRun")} />}
      {failure && <Notice tone="danger" title={failure} />}
      {run && <>
        <div className="flex flex-wrap items-center gap-2">
          <strong className="font-semibold text-fg-default">{t("batchUpgrade.target", { version: run.target_version })}</strong>
          <Badge size="sm" status="neutral">{t(`batchUpgrade.runState.${run.state}`, { defaultValue: t("batchUpgrade.runState.unknown") })}</Badge>
        </div>
        <UpgradeSummary counts={run.counts} />
        {run.state === "paused" && <Notice tone="warning" title={t(run.pause_reason === "repeated_unknown_outcome" ? "batchUpgrade.pausedRepeatedUnknown" : "batchUpgrade.pausedArtifact")} />}
        {run.blocked_by_item && run.state !== "paused" && <Notice tone={run.blocked_by_item.reason === "unconfirmed" ? "info" : "warning"} title={t(run.blocked_by_item.reason === "unconfirmed" ? "batchUpgrade.blocked" : "batchUpgrade.activeBlock", { name: run.blocked_by_item.name || run.blocked_by_item.node_uuid })} />}
        {isAdmin && (canStop || canContinue) && <div className="flex flex-wrap gap-2">
          {canStop && <ConfirmPopover title={<span className="inline-flex items-center gap-1">{t("batchUpgrade.stopConfirm")}<HelpTip content={t("batchUpgrade.stopHelp")} /></span>}
            onConfirm={stop} confirmText={t("batchUpgrade.stop")} cancelText={t("batchUpgrade.close")}
            trigger={<Button variant="secondary" disabled={busy}>{t("batchUpgrade.stop")}</Button>} />}
          {canContinue && <Button variant="secondary" disabled={busy || continueBlocked} onClick={() => onContinue(runID)}>{t("batchUpgrade.continue")}</Button>}
        </div>}
      </>}
      <div className="flex flex-wrap items-center gap-2">
        <div className="w-[220px]">
          <OptionSelect aria-label={t("batchUpgrade.filter")} value={phase} onChange={(value) => { setPhase(value || ALL); setPage(1); }} options={[
            { value: ALL, label: t("batchUpgrade.allResults") },
            ...phases.map((value) => ({ value, label: t(`batchUpgrade.phase.${value}`) })),
          ]} />
        </div>
        <Button variant="secondary" onClick={refresh}>{t("batchUpgrade.refresh")}</Button>
      </div>
      <div className="relative" aria-busy={switching || undefined}>
      {switching && rows.length > 0 && <div className="absolute inset-0 z-10 flex items-start justify-center bg-surface-floating/60 pt-10"><Spinner label={t("batchUpgrade.loadingRun")} /></div>}
      <Table size="sm" className="table-fixed" containerClassName="max-w-full overflow-x-auto" style={{ width: TABLE_WIDTH, minWidth: "100%", tableLayout: "fixed" }}>
        <TableHeader>
          <TableRow>
            <TableHead style={{ width: COLUMN_WIDTHS.node }}>{t("batchUpgrade.columns.node")}</TableHead>
            <TableHead style={{ width: COLUMN_WIDTHS.from }}>{t("batchUpgrade.columns.from")}</TableHead>
            <TableHead style={{ width: COLUMN_WIDTHS.current }}>{t("batchUpgrade.columns.current")}</TableHead>
            <TableHead style={{ width: COLUMN_WIDTHS.phase }}>{t("batchUpgrade.columns.phase")}</TableHead>
            <TableHead style={{ width: COLUMN_WIDTHS.reason }}>{t("batchUpgrade.columns.reason")}</TableHead>
            <TableHead style={{ width: COLUMN_WIDTHS.time }}>{t("batchUpgrade.columns.time")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {items.loading && rows.length === 0 && <TableRow><TableCell colSpan={6}><Spinner label={t("batchUpgrade.loadingRun")} /></TableCell></TableRow>}
          {!items.loading && rows.length === 0 && <TableRow><TableCell colSpan={6} className="text-center text-fg-muted">{t("batchUpgrade.noItems")}</TableCell></TableRow>}
          {rows.map((item, index) => <TableRow key={item.node_uuid} index={index} data-row="result">
            <TableCell className="truncate" title={`${item.name || item.node_uuid}\n${item.node_uuid}`}>{item.name || item.node_uuid}</TableCell>
            <TableCell>{version(item.from_version)}</TableCell>
            <TableCell>{version(item.current_version || item.observed_version)}</TableCell>
            <TableCell><Badge size="sm" status="neutral">{item.phase === "unconfirmed" && item.verified_at
              ? t(`batchUpgrade.settled.${item.verification_code}`, { defaultValue: t("batchUpgrade.phase.unconfirmed") })
              : t(`batchUpgrade.phase.${item.phase}`, { defaultValue: t("batchUpgrade.phase.unknown") })}</Badge></TableCell>
            <TableCell>
              <div className="flex flex-col [overflow-wrap:anywhere]">
                {!item.reason && !item.error && !item.verified_at && !item.verification_note && "—"}
                {item.reason && !(item.phase === "unconfirmed" && item.verified_at) && <span>{t(`batchUpgrade.reason.${item.reason}`, { defaultValue: t("batchUpgrade.reason.unavailable") })}</span>}
                {/* A skip is fully said by its reason code; the server's English note would repeat it in red. */}
                {item.error && !(item.phase === "skipped" && item.reason && i18n.exists(`nodes:batchUpgrade.reason.${item.reason}`)) && <span className="text-fg-danger">{item.error}</span>}
                {item.verified_at && <span>{[item.verification_code && item.verification_code !== "operator_verified" ? t(`batchUpgrade.verification.${item.verification_code}`, { defaultValue: "" }) : "", t("batchUpgrade.verifiedAt", { at: formatTimeSec(item.verified_at) })].filter(Boolean).join(" ")}</span>}
                {item.verification_note && !systemNoteCodes.has(item.verification_code ?? "") && <span>{t("batchUpgrade.verified", { note: item.verification_note })}</span>}
              </div>
            </TableCell>
            <TableCell><span className="whitespace-nowrap">{timeRange(item.started_at, item.finished_at)}</span></TableCell>
          </TableRow>)}
        </TableBody>
      </Table>
      </div>
      {(items.data?.total ?? 0) > PAGE_SIZE && <Pager page={page} pageSize={PAGE_SIZE} total={items.data?.total ?? 0} onChange={setPage} />}
    </div>
  </SideDrawer>;
}

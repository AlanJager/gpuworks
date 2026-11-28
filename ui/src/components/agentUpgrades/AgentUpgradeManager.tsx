import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, useEvents, usePolling } from "../../api";
import { newAgentUpgradeKey, useAgentUpgradeAction } from "../../agentUpgrades";
import { agentUpgradeRunning, type AgentUpgradePreview } from "../../agentUpgradesTypes";
import UpgradePreviewModal from "./UpgradePreviewModal";
import UpgradeRunDrawer from "./UpgradeRunDrawer";
import UpgradeSummary from "./UpgradeSummary";
import EmptyState from "../EmptyState";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../ui/dialog";
import { Notice } from "../../ui-kit/Notice";
import { OptionSelect } from "../../ui-kit/OptionSelect";
import { Spinner } from "../../ui-kit/Spinner";

/** upgradeAllRequest counts the times the page asked to upgrade every node; each new value opens the whole-fleet preview. */
export default function AgentUpgradeManager({ isAdmin, requestedRunID, selectedNodeUUIDs, upgradeAllRequest = 0, onStarted }: { isAdmin: boolean; requestedRunID?: number; selectedNodeUUIDs: string[]; upgradeAllRequest?: number; onStarted?: () => void }) {
  const { t } = useTranslation("nodes");
  const [watch, setWatch] = useState(false);
  const runs = usePolling(() => api.agentUpgradeRuns(), watch ? 5000 : 0, [watch]);
  const refresh = () => { void runs.refresh(); };
  useEvents("agent_upgrade", refresh);
  useEffect(() => {
    setWatch(!!runs.data?.blocked_by_item || !!runs.data?.items.some((run) => agentUpgradeRunning(run) || !!run.blocked_by_item));
  }, [runs.data]);
  const [runID, setRunID] = useState<number>();
  const refreshRef = useRef(runs.refresh);
  refreshRef.current = runs.refresh;
  useEffect(() => {
    if (requestedRunID) { setRunID(requestedRunID); void refreshRef.current(); }
  }, [requestedRunID]);
  const [historyOpen, setHistoryOpen] = useState(false);
  const selectedSnapshot = useRef<string[]>([]);
  const [previewOpen, setPreviewOpen] = useState(false);
  const [preview, setPreview] = useState<AgentUpgradePreview | null>(null);
  const [sourceRun, setSourceRun] = useState<number>();
  const [previewLoading, setPreviewLoading] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const key = useRef("");
  const perform = useAgentUpgradeAction();
  const loadPreview = (source?: number, all = false) => void perform(async () => {
    setHistoryOpen(false); setSourceRun(source); setPreviewOpen(true); setPreview(null); setError(""); setPreviewLoading(true);
    try {
      const result = source ? await api.continueAgentUpgradePreview(source) : await api.agentUpgradePreview(all ? { scope: "all" } : { node_uuids: [...selectedSnapshot.current] });
      // The whole-fleet preview fixes its range like a selection does: previewing again asks about these nodes.
      if (all) selectedSnapshot.current = [...result.node_uuids];
      key.current = newAgentUpgradeKey(); setPreview(result);
    } catch (e) { setError((e as Error).message); }
    finally { setPreviewLoading(false); }
  });
  const start = () => void perform(async () => {
    if (!preview || !isAdmin) return;
    setBusy(true); setError("");
    try {
      const run = await api.createAgentUpgrade({
        node_uuids: [...preview.node_uuids],
        expected_target_version: preview.target_version,
        expected_manifest_fingerprint: preview.manifest_fingerprint,
        idempotency_key: key.current,
        ...(sourceRun ? { source_run_id: sourceRun } : {}),
      });
      setPreviewOpen(false); setPreview(null); setRunID(run.id); onStarted?.(); refresh();
    } catch (e) { setError((e as Error).message); }
    finally { setBusy(false); }
  });
  const latest = runs.data?.items.find(agentUpgradeRunning) ?? runs.data?.items[0];
  const blocker = runs.data?.blocked_by_item ?? latest?.blocked_by_item;
  // A blocked fleet cannot start a batch; the history dialog names what blocks it.
  const upgradeAll = useRef(() => {});
  upgradeAll.current = () => { if (blocker) { setHistoryOpen(true); refresh(); } else loadPreview(undefined, true); };
  useEffect(() => { if (isAdmin && upgradeAllRequest) upgradeAll.current(); }, [isAdmin, upgradeAllRequest]);
  return <>
    <div className="flex flex-wrap gap-2">
      {isAdmin && <Button variant="secondary" disabled={!selectedNodeUUIDs.length || previewLoading || busy || !!blocker} onClick={() => {
        selectedSnapshot.current = [...selectedNodeUUIDs]; loadPreview();
      }}>{t("batchUpgrade.selected")}</Button>}
      <Button variant="secondary" onClick={() => { setHistoryOpen(true); refresh(); }}>{t("batchUpgrade.history")}</Button>
    </div>
    <Dialog open={historyOpen} onOpenChange={(next) => { if (!next) setHistoryOpen(false); }}>
      <DialogContent size="lg" closeButtonLabel={t("batchUpgrade.close")} className="flex max-h-[calc(100dvh-32px)] w-[min(850px,calc(100vw-32px))] max-w-none flex-col gap-4">
        <DialogHeader><DialogTitle>{t("batchUpgrade.history")}</DialogTitle></DialogHeader>
        <div className="flex min-h-0 flex-col gap-3 overflow-y-auto">
          {runs.loading && <Spinner label={t("batchUpgrade.loadingRuns")} />}
          {runs.error && <Notice tone="danger" title={runs.error.message} />}
          {blocker && <Notice tone={blocker.reason === "unconfirmed" ? "info" : "warning"} title={t(blocker.reason === "unconfirmed" ? "batchUpgrade.blocked" : "batchUpgrade.activeBlock", { name: blocker.name || blocker.node_uuid })}
            action={<Button size="sm" variant="secondary" onClick={() => { setRunID(blocker.run_id); setHistoryOpen(false); }}>{t("batchUpgrade.details")}</Button>} />}
          {latest ? <>
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-fg-default">{t("batchUpgrade.target", { version: latest.target_version })}</span>
              <Badge size="sm" status="neutral">{t(`batchUpgrade.runState.${latest.state}`, { defaultValue: t("batchUpgrade.runState.unknown") })}</Badge>
              <Button size="sm" variant="secondary" onClick={() => { setRunID(latest.id); setHistoryOpen(false); }}>{t("batchUpgrade.details")}</Button>
            </div>
            <UpgradeSummary counts={latest.counts} />
          </> : !runs.loading && !runs.error && <EmptyState compact description={t("batchUpgrade.empty")} />}
          {!!runs.data?.items.length && <div className="w-[260px]">
            <OptionSelect aria-label={t("batchUpgrade.history")} placeholder={t("batchUpgrade.history")} value={runID === undefined ? undefined : String(runID)}
              onChange={(id) => { if (id) { setRunID(Number(id)); setHistoryOpen(false); } }}
              options={runs.data.items.map((run) => ({ value: String(run.id), label: t("batchUpgrade.historyItem", { id: run.id, version: run.target_version }) }))} />
          </div>}
        </div>
        <DialogFooter><Button variant="secondary" onClick={refresh}>{t("batchUpgrade.refresh")}</Button></DialogFooter>
      </DialogContent>
    </Dialog>
    <UpgradePreviewModal open={previewOpen} preview={preview} loading={previewLoading} busy={busy} error={error} onClose={() => setPreviewOpen(false)} onRefresh={() => loadPreview(sourceRun)} onStart={start} />
    {runID !== undefined && <UpgradeRunDrawer key={runID} runID={runID} isAdmin={isAdmin} onClose={() => setRunID(undefined)} onContinue={loadPreview} onChanged={refresh} />}
  </>;
}

import { Alert, Button, Card, Empty, Select, Space, Spin, Typography } from "antd";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, useEvents, usePolling } from "../../api";
import { newAgentUpgradeKey, useAgentUpgradeAction } from "../../agentUpgrades";
import { agentUpgradeRunning, type AgentUpgradePreview } from "../../agentUpgradesTypes";
import UpgradePreviewModal from "./UpgradePreviewModal";
import UpgradeRunDrawer from "./UpgradeRunDrawer";
import UpgradeSummary from "./UpgradeSummary";

export default function AgentUpgradeManager({ isAdmin, requestedRunID }: { isAdmin: boolean; requestedRunID?: number }) {
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
  const [previewOpen, setPreviewOpen] = useState(false);
  const [preview, setPreview] = useState<AgentUpgradePreview | null>(null);
  const [sourceRun, setSourceRun] = useState<number>();
  const [previewLoading, setPreviewLoading] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const key = useRef("");
  const perform = useAgentUpgradeAction();
  const loadPreview = (source?: number) => void perform(async () => {
    setSourceRun(source); setPreviewOpen(true); setPreview(null); setError(""); setPreviewLoading(true);
    try {
      const result = source ? await api.continueAgentUpgradePreview(source) : await api.agentUpgradePreview({ scope: "all" });
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
      setPreviewOpen(false); setPreview(null); setRunID(run.id); refresh();
    } catch (e) { setError((e as Error).message); }
    finally { setBusy(false); }
  });
  const latest = runs.data?.items.find(agentUpgradeRunning) ?? runs.data?.items[0];
  const blocker = runs.data?.blocked_by_item ?? latest?.blocked_by_item;
  return <>
    <Card title={t("batchUpgrade.title")} style={{ marginBottom: 16 }} extra={<Space wrap>
      <Button onClick={refresh}>{t("batchUpgrade.refresh")}</Button>
      {isAdmin && <Button type="primary" disabled={previewLoading || busy || !!blocker} onClick={() => loadPreview()}>{t("batchUpgrade.all")}</Button>}
    </Space>}>
      <Space direction="vertical" style={{ width: "100%" }}>
        {runs.loading && <Spin aria-label={t("batchUpgrade.loadingRuns")} />}
        {runs.error && <Alert type="error" showIcon message={runs.error.message} />}
        {blocker && <Alert type="warning" showIcon message={t(blocker.reason === "unconfirmed" ? "batchUpgrade.blocked" : "batchUpgrade.activeBlock", { name: blocker.name || blocker.node_uuid })} action={<Button size="small" onClick={() => setRunID(blocker.run_id)}>{t("batchUpgrade.details")}</Button>} />}
        {latest ? <>
          <Space wrap><Typography.Text>{t("batchUpgrade.target", { version: latest.target_version })}</Typography.Text><Typography.Text>{t(`batchUpgrade.runState.${latest.state}`, { defaultValue: t("batchUpgrade.runState.unknown") })}</Typography.Text><Button onClick={() => setRunID(latest.id)}>{t("batchUpgrade.details")}</Button></Space>
          <UpgradeSummary counts={latest.counts} />
        </> : !runs.loading && !runs.error && <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t("batchUpgrade.empty")} />}
        {!!runs.data?.items.length && <Select aria-label={t("batchUpgrade.history")} placeholder={t("batchUpgrade.history")} style={{ minWidth: 260 }} value={runID} onChange={setRunID} options={runs.data.items.map((run) => ({ value: run.id, label: t("batchUpgrade.historyItem", { id: run.id, version: run.target_version }) }))} />}
      </Space>
    </Card>
    <UpgradePreviewModal open={previewOpen} preview={preview} loading={previewLoading} busy={busy} error={error} onClose={() => setPreviewOpen(false)} onRefresh={() => loadPreview(sourceRun)} onStart={start} />
    {runID !== undefined && <UpgradeRunDrawer key={runID} runID={runID} isAdmin={isAdmin} onClose={() => setRunID(undefined)} onContinue={loadPreview} onChanged={refresh} />}
  </>;
}

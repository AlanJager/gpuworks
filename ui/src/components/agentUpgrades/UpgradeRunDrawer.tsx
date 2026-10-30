import { Alert, Button, Drawer, Input, Modal, Popconfirm, Select, Space, Spin, Table, Tag, Typography } from "antd";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, useEvents, usePolling } from "../../api";
import { agentUpgradeRunning, type AgentUpgradeItem, type AgentUpgradeRun } from "../../agentUpgradesTypes";
import { formatTimeSec } from "../../i18n/datetime";
import UpgradeSummary from "./UpgradeSummary";
import { useAgentUpgradeAction } from "../../agentUpgrades";

const phases = ["queued", "transferring", "waiting_registration", "observing_recovery", "succeeded", "failed", "skipped", "cancelled", "interrupted", "unconfirmed"];

export default function UpgradeRunDrawer({ runID, isAdmin, onClose, onContinue, onChanged }: {
  runID: number;
  isAdmin: boolean;
  onClose: () => void;
  onContinue: (id: number) => void;
  onChanged: () => void;
}) {
  const { t } = useTranslation("nodes");
  const [watch, setWatch] = useState(true);
  const interval = watch ? 5000 : 0;
  const status = usePolling(() => api.agentUpgradeRun(runID), interval, [runID, interval]);
  const run: AgentUpgradeRun | null = status.data;
  useEffect(() => { if (run) setWatch(agentUpgradeRunning(run) || run.blocked_by_item?.run_id === run.id); }, [run]);
  const [page, setPage] = useState(1);
  const [phase, setPhase] = useState("");
  const items = usePolling(() => api.agentUpgradeItems(runID, page, 20, phase), interval, [runID, page, phase, interval]);
  const refresh = () => { void status.refresh(); void items.refresh(); onChanged(); };
  useEvents("agent_upgrade", refresh);
  const [busy, setBusy] = useState(false);
  const perform = useAgentUpgradeAction();
  const [error, setError] = useState("");
  const [release, setRelease] = useState<AgentUpgradeItem | null>(null);
  const [note, setNote] = useState("");
  const [noteError, setNoteError] = useState(false);
  const stop = () => perform(async () => {
    if (busy) return;
    setBusy(true); setError("");
    try { await api.stopAgentUpgrade(runID); refresh(); }
    catch (e) { setError((e as Error).message); }
    finally { setBusy(false); }
  });
  const confirmRelease = () => perform(async () => {
    if (!release || busy) return;
    if (!note.trim()) { setNoteError(true); return; }
    setBusy(true); setError("");
    try {
      await api.releaseAgentUpgrade(runID, release.node_uuid, note.trim());
      setRelease(null); refresh();
    } catch (e) { setError((e as Error).message); }
    finally { setBusy(false); }
  });
  const showActions = isAdmin && !!items.data?.items.some((item) => item.phase === "unconfirmed" && !item.verified_at);
  const renderVersion = (value?: string) => <Typography.Text style={{ overflowWrap: "anywhere" }}>{value || "—"}</Typography.Text>;
  return <>
    <Drawer open title={t("batchUpgrade.runTitle", { id: runID })} width="min(1280px, calc(100vw - 32px))" onClose={onClose}>
      <Space direction="vertical" style={{ width: "100%" }} size="middle">
        <Button onClick={refresh}>{t("batchUpgrade.refresh")}</Button>
        {status.loading && !run && <Spin aria-label={t("batchUpgrade.loadingRun")} />}
        {(status.error || items.error || error) && <Alert type="error" showIcon message={error || status.error?.message || items.error?.message} />}
        {run && <>
          <Space wrap><Typography.Text strong>{t("batchUpgrade.target", { version: run.target_version })}</Typography.Text><Tag>{t(`batchUpgrade.runState.${run.state}`, { defaultValue: t("batchUpgrade.runState.unknown") })}</Tag></Space>
          <UpgradeSummary counts={run.counts} />
          {run.blocked_by_item && <Alert type="warning" showIcon message={t(run.blocked_by_item.reason === "unconfirmed" ? "batchUpgrade.blocked" : "batchUpgrade.activeBlock", { name: run.blocked_by_item.name || run.blocked_by_item.node_uuid })} description={t(run.blocked_by_item.reason === "unconfirmed" ? "batchUpgrade.unconfirmedHelp" : "batchUpgrade.activeHelp")} />}
          <Space wrap>
            {isAdmin && (run.counts.queued > 0 || run.counts.active > 0) && !run.stop_requested_at && <Popconfirm title={t("batchUpgrade.stopConfirm")} description={t("batchUpgrade.stopHelp")} onConfirm={() => void stop()} okText={t("batchUpgrade.stop")} cancelText={t("batchUpgrade.close")}>
              <Button disabled={busy}>{t("batchUpgrade.stop")}</Button>
            </Popconfirm>}
            {isAdmin && (run.counts.failed + run.counts.interrupted + run.counts.cancelled + run.counts.queued > 0) && (run.state === "paused" || !agentUpgradeRunning(run)) &&
              <Button disabled={busy || (!!run.blocked_by_item && !(["paused", "interrupted"].includes(run.state) && run.counts.active === 0 && run.blocked_by_item.run_id === run.id && run.blocked_by_item.reason === "active_run"))} onClick={() => onContinue(runID)}>{t("batchUpgrade.continue")}</Button>}
          </Space>
        </>}
        <Select value={phase} style={{ minWidth: 220 }} aria-label={t("batchUpgrade.filter")} onChange={(value) => { setPhase(value); setPage(1); }} options={[
          { value: "", label: t("batchUpgrade.allResults") },
          ...phases.map((value) => ({ value, label: t(`batchUpgrade.phase.${value}`) })),
        ]} />
        <Table<AgentUpgradeItem> size="small" rowKey="node_uuid" loading={items.loading} dataSource={items.data?.items ?? []}
          tableLayout="fixed" scroll={{ x: showActions ? 1324 : 1164 }}
          locale={{ emptyText: t("batchUpgrade.noItems") }}
          pagination={{ current: page, pageSize: 20, total: items.data?.total ?? 0, showSizeChanger: false, onChange: setPage }}
          columns={[
            { title: t("batchUpgrade.columns.node"), key: "node", width: 220, render: (_, item) => <div style={{ overflowWrap: "anywhere" }}><Typography.Text style={{ display: "block" }}>{item.name || item.node_uuid}</Typography.Text><Typography.Text type="secondary">{item.node_uuid}</Typography.Text></div> },
            { title: t("batchUpgrade.columns.from"), dataIndex: "from_version", width: 150, render: renderVersion },
            { title: t("batchUpgrade.columns.current"), key: "current", width: 150, render: (_, item) => renderVersion(item.current_version || item.observed_version) },
            { title: t("batchUpgrade.columns.phase"), dataIndex: "phase", width: 144, render: (value: string) => <Tag>{t(`batchUpgrade.phase.${value}`, { defaultValue: t("batchUpgrade.phase.unknown") })}</Tag> },
            { title: t("batchUpgrade.columns.reason"), key: "reason", width: 280, render: (_, item) => <Space direction="vertical" size={0} style={{ width: "100%", overflowWrap: "anywhere" }}>
              {!item.reason && !item.error && !item.verified_at && !item.verification_note && "—"}
              {item.reason && <Typography.Text>{t(`batchUpgrade.reason.${item.reason}`, { defaultValue: t("batchUpgrade.reason.unavailable") })}</Typography.Text>}
              {item.error && <Typography.Text type="danger">{item.error}</Typography.Text>}
              {item.verified_at && <Typography.Text>{t("batchUpgrade.verifiedAt", { at: formatTimeSec(item.verified_at) })}</Typography.Text>}
              {item.verification_note && <Typography.Text>{t("batchUpgrade.verified", { note: item.verification_note })}</Typography.Text>}
            </Space> },
            { title: t("batchUpgrade.columns.time"), key: "time", width: 220, render: (_, item) => <Space direction="vertical" size={0} style={{ whiteSpace: "nowrap" }}>
              <Typography.Text>{t("batchUpgrade.startedAt", { at: formatTimeSec(item.started_at) })}</Typography.Text>
              <Typography.Text>{t("batchUpgrade.finishedAt", { at: formatTimeSec(item.finished_at) })}</Typography.Text>
            </Space> },
            { title: t("batchUpgrade.columns.action"), key: "action", width: 160, fixed: "right", hidden: !showActions, render: (_, item) => isAdmin && item.phase === "unconfirmed" && !item.verified_at ? <Button size="small" onClick={() => { setRelease(item); setNote(""); setNoteError(false); }}>{t("batchUpgrade.release")}</Button> : "—" },
          ]} />
      </Space>
    </Drawer>
    <Modal open={!!release} title={t("batchUpgrade.releaseTitle", { name: release?.name || release?.node_uuid })} onCancel={() => !busy && setRelease(null)}
      onOk={() => void confirmRelease()} confirmLoading={busy} okButtonProps={{ "aria-label": t("batchUpgrade.release") }} okText={t("batchUpgrade.release")} cancelText={t("batchUpgrade.close")}>
      <Typography.Paragraph>{t("batchUpgrade.releaseHelp")}</Typography.Paragraph>
      <Input.TextArea aria-label={t("batchUpgrade.releaseNote")} placeholder={t("batchUpgrade.releaseNote")} value={note} onChange={(e) => { setNote(e.target.value); setNoteError(false); }} status={noteError ? "error" : undefined} />
      {noteError && <Typography.Text type="danger">{t("batchUpgrade.noteRequired")}</Typography.Text>}
      {error && <Alert type="error" message={error} />}
    </Modal>
  </>;
}

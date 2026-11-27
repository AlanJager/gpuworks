import { Alert, Button, Modal, Space, Spin, Table, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { Badge, type BadgeProps } from "../ui/badge";
import type { AgentUpgradeCandidate, AgentUpgradePreview } from "../../agentUpgradesTypes";
import { shortBuild } from "../../buildId";

const kindTone: Record<string, Pick<BadgeProps, "status" | "color">> = { stable: { status: "success" }, prerelease: { status: "warning" }, development: { color: "orange" }, unknown: { color: "gray" } };

// The batch upgrades the nodes it can and records why it skips the rest, so
// the dialog leads with what will happen and starts in one click.
export default function UpgradePreviewModal({ open, preview, loading, busy, error, onClose, onRefresh, onStart }: {
  open: boolean;
  preview: AgentUpgradePreview | null;
  loading: boolean;
  busy: boolean;
  error: string;
  onClose: () => void;
  onRefresh: () => void;
  onStart: () => void;
}) {
  const { t, i18n } = useTranslation("nodes");
  const { t: tc } = useTranslation("common");
  const eligible = preview?.counts.eligible ?? 0;
  const skipped = (preview?.node_uuids.length ?? 0) - eligible;
  const build = preview?.target_build?.version === preview?.target_version ? preview?.target_build : undefined;
  const kind = build && ["stable", "prerelease", "development"].includes(build.kind) ? build.kind : "unknown";
  // The nodes that will be upgraded come first; the skipped ones follow with their reason.
  const nodes = preview ? [...preview.nodes].sort((a, b) => Number(b.eligibility === "eligible") - Number(a.eligibility === "eligible")) : [];
  const title = preview
    ? <span className="flex flex-wrap items-center gap-2">{t("batchUpgrade.previewTitle", { version: preview.target_version })}<Badge size="sm" {...kindTone[kind]}>{tc(`buildInfo.kind.${kind}`)}</Badge></span>
    : t("batchUpgrade.selected");
  return <Modal open={open} title={title} onCancel={() => !busy && onClose()} destroyOnHidden footer={<Space>
      <Button disabled={busy} onClick={onClose}>{t("batchUpgrade.close")}</Button>
      <Button disabled={loading || busy} onClick={onRefresh}>{t("batchUpgrade.refreshPreview")}</Button>
      {eligible > 0 && <Button type="primary" aria-label={t("batchUpgrade.start", { count: eligible })} loading={busy} disabled={loading || !!preview?.blocked_by_item} onClick={onStart}>
        {t("batchUpgrade.start", { count: eligible })}
      </Button>}
    </Space>} width={850}>
    <Space direction="vertical" style={{ width: "100%" }}>
      {loading && <Spin aria-label={t("batchUpgrade.loadingPreview")} />}
      {error && <Alert type="error" showIcon message={error} />}
      {preview && <>
        {preview.blocked_by_item && <Alert type={preview.blocked_by_item.reason === "unconfirmed" ? "info" : "warning"} showIcon message={t(preview.blocked_by_item.reason === "unconfirmed" ? "batchUpgrade.blocked" : "batchUpgrade.activeBlock", { name: preview.blocked_by_item.name || preview.blocked_by_item.node_uuid })} />}
        {eligible > 0
          ? <Alert type="warning" showIcon message={<>{t(skipped > 0 ? "batchUpgrade.summary" : "batchUpgrade.summaryAll", { eligible, skipped })} {t("batchUpgrade.warning.interrupt")}</>} />
          : <Alert type="info" showIcon message={t("batchUpgrade.noEligible")} />}
        <Table<AgentUpgradeCandidate> size="small" rowKey="node_uuid" dataSource={nodes} tableLayout="fixed" pagination={{ pageSize: 10, showSizeChanger: false, hideOnSinglePage: true }} columns={[
          { title: t("batchUpgrade.columns.node"), key: "node", render: (_, node) => <Typography.Text ellipsis={{ tooltip: node.name || node.node_uuid }}>{node.name || node.node_uuid}</Typography.Text> },
          { title: t("batchUpgrade.columns.version"), key: "version", width: 320, render: (_, node) => node.eligibility === "eligible"
            /* Both ends of each id stay readable on one line; the full pair is on hover. */
            ? <div className="flex flex-col items-start gap-1"><span className="whitespace-nowrap" title={`${node.from_version} → ${preview.target_version}`}>{shortBuild(node.from_version)} → {shortBuild(preview.target_version)}</span>
                <Badge size="sm" status="neutral">{t(node.downgrade ? "fleet.status.downgrade" : node.version_change === "upgrade" ? "fleet.willUpgrade" : "fleet.replaceBuild")}</Badge></div>
            : <span className="whitespace-nowrap" title={node.from_version}>{shortBuild(node.from_version) || "—"}</span> },
          { title: t("batchUpgrade.columns.status"), key: "status", width: 200, render: (_, node) => node.eligibility === "eligible"
            ? t("batchUpgrade.eligibility.eligible")
            : <div className="flex flex-col"><span>{t(`batchUpgrade.eligibility.${node.eligibility}`, { defaultValue: t("batchUpgrade.eligibility.unavailable") })}</span>
                {/* The preview's reason is a code only for some cases; free text stays out of a localized table. */}
                {node.reason && node.reason !== node.eligibility && i18n.exists(`nodes:batchUpgrade.reason.${node.reason}`) && <Typography.Text type="secondary">{t(`batchUpgrade.reason.${node.reason}`)}</Typography.Text>}</div> },
        ]} />
      </>}
    </Space>
  </Modal>;
}

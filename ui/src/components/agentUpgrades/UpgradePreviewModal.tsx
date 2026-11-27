import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Badge, type BadgeProps } from "../ui/badge";
import { Button } from "../ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../ui/dialog";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import { Notice } from "../../ui-kit/Notice";
import { Pager } from "../../ui-kit/Pager";
import { Spinner } from "../../ui-kit/Spinner";
import type { AgentUpgradePreview } from "../../agentUpgradesTypes";
import { shortBuild } from "../../buildId";

const kindTone: Record<string, Pick<BadgeProps, "status" | "color">> = { stable: { status: "success" }, prerelease: { status: "warning" }, development: { color: "orange" }, unknown: { color: "gray" } };

const PAGE_SIZE = 10;
/** Fixed widths keep the columns where they are from one page to the next. */
const COLUMN_WIDTHS = { version: 320, status: 200 };

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
  const [page, setPage] = useState(1);
  useEffect(() => setPage(1), [preview]);
  const eligible = preview?.counts.eligible ?? 0;
  const skipped = (preview?.node_uuids.length ?? 0) - eligible;
  const build = preview?.target_build?.version === preview?.target_version ? preview?.target_build : undefined;
  const kind = build && ["stable", "prerelease", "development"].includes(build.kind) ? build.kind : "unknown";
  // The nodes that will be upgraded come first; the skipped ones follow with their reason.
  const nodes = preview ? [...preview.nodes].sort((a, b) => Number(b.eligibility === "eligible") - Number(a.eligibility === "eligible")) : [];
  const shown = nodes.slice((page - 1) * PAGE_SIZE, page * PAGE_SIZE);
  const startLabel = t("batchUpgrade.start", { count: eligible });
  return <Dialog open={open} onOpenChange={(next) => { if (!next && !busy) onClose(); }}>
    <DialogContent size="lg" closeButtonLabel={t("batchUpgrade.close")} closeButtonDisabled={busy}
      className="flex max-h-[calc(100dvh-32px)] w-[min(850px,calc(100vw-32px))] max-w-none flex-col gap-4">
      <DialogHeader>
        <DialogTitle className="flex flex-wrap items-center gap-2 pr-8">
          {preview ? <>{t("batchUpgrade.previewTitle", { version: preview.target_version })}<Badge size="sm" {...kindTone[kind]}>{tc(`buildInfo.kind.${kind}`)}</Badge></> : t("batchUpgrade.selected")}
        </DialogTitle>
      </DialogHeader>
      <div className="flex min-h-0 flex-col gap-3 overflow-y-auto">
        {loading && <Spinner label={t("batchUpgrade.loadingPreview")} />}
        {error && <Notice tone="danger" title={error} />}
        {preview && <>
          {preview.blocked_by_item && <Notice tone={preview.blocked_by_item.reason === "unconfirmed" ? "info" : "warning"} title={t(preview.blocked_by_item.reason === "unconfirmed" ? "batchUpgrade.blocked" : "batchUpgrade.activeBlock", { name: preview.blocked_by_item.name || preview.blocked_by_item.node_uuid })} />}
          {eligible > 0
            ? <Notice tone="warning" title={<>{t(skipped > 0 ? "batchUpgrade.summary" : "batchUpgrade.summaryAll", { eligible, skipped })} {t("batchUpgrade.warning.interrupt")}</>} />
            : <Notice tone="info" title={t("batchUpgrade.noEligible")} />}
          <Table size="sm" className="table-fixed">
            <TableHeader>
              <TableRow>
                <TableHead>{t("batchUpgrade.columns.node")}</TableHead>
                <TableHead style={{ width: COLUMN_WIDTHS.version }}>{t("batchUpgrade.columns.version")}</TableHead>
                <TableHead style={{ width: COLUMN_WIDTHS.status }}>{t("batchUpgrade.columns.status")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((node, index) => {
                const upgrades = node.eligibility === "eligible";
                return <TableRow key={node.node_uuid} index={index}>
                  <TableCell className="truncate text-fg-default" title={node.name || node.node_uuid}>{node.name || node.node_uuid}</TableCell>
                  <TableCell className="text-fg-default">
                    {/* Both ends of each id stay readable on one line; the full pair is on hover. */}
                    {upgrades
                      ? <div className="flex flex-col items-start gap-1">
                          <span className="whitespace-nowrap" title={`${node.from_version} → ${preview.target_version}`}>{shortBuild(node.from_version)} → {shortBuild(preview.target_version)}</span>
                          <Badge size="sm" status="neutral">{t(node.downgrade ? "fleet.status.downgrade" : node.version_change === "upgrade" ? "fleet.willUpgrade" : "fleet.replaceBuild")}</Badge>
                        </div>
                      : <span className="whitespace-nowrap" title={node.from_version}>{shortBuild(node.from_version) || "—"}</span>}
                  </TableCell>
                  <TableCell className="text-fg-default">
                    {upgrades
                      ? t("batchUpgrade.eligibility.eligible")
                      : <div className="flex flex-col">
                          <span>{t(`batchUpgrade.eligibility.${node.eligibility}`, { defaultValue: t("batchUpgrade.eligibility.unavailable") })}</span>
                          {/* The preview's reason is a code only for some cases; free text stays out of a localized table. */}
                          {node.reason && node.reason !== node.eligibility && i18n.exists(`nodes:batchUpgrade.reason.${node.reason}`) && <span className="text-label text-fg-muted">{t(`batchUpgrade.reason.${node.reason}`)}</span>}
                        </div>}
                  </TableCell>
                </TableRow>;
              })}
            </TableBody>
          </Table>
          {nodes.length > PAGE_SIZE && <Pager page={page} pageSize={PAGE_SIZE} total={nodes.length} onChange={setPage} />}
        </>}
      </div>
      <DialogFooter>
        <Button variant="secondary" disabled={busy} onClick={onClose}>{t("batchUpgrade.close")}</Button>
        <Button variant="secondary" disabled={loading || busy} onClick={onRefresh}>{t("batchUpgrade.refreshPreview")}</Button>
        {eligible > 0 && <Button variant="primary" aria-label={startLabel} loading={busy} disabled={loading || !!preview?.blocked_by_item} onClick={onStart}>{startLabel}</Button>}
      </DialogFooter>
    </DialogContent>
  </Dialog>;
}

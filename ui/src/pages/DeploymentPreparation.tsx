import DetailSection from "../components/detail/DetailSection";
import StartupStatusIcon, { type StartupStatus } from "./StartupStatusIcon";
import { Space, Tooltip, Typography, message } from "antd";
import { Button, Progress } from "../pages/DeploymentControls";
import { storageDetailPath } from "./storage/storageLinks";
import { useEffect, useState } from "react";
import { apiErrorText } from "../apiError";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { api, type Deployment, type ModelNodePresence } from "../api";
import DeploymentImageProgress from "./DeploymentImageProgress";
import { formatBytes, remainingText } from "../format";
import { isModelFileReady } from "../storageTypes";
import { rateLabel, transferEtaSeconds, transferOrigin } from "./models/modelPresenceDisplay";
import { HelpTip } from "../ui-kit/HelpTip";

// All classification, timing and what may be retried comes from the server's
// preparation view; the server also zeroes a rate that is not meaningful.
// Unknown stages and old agents deliberately have no synthetic percentage.
export function PreparationLabel({ item }: { item: ModelNodePresence }) {
  const { t } = useTranslation("deployments");
  const view = item.preparation;
  const key = isModelFileReady(item.state) ? "preparation.phase.ready"
    : view?.reason ? `preparation.reason.${view.reason}` : `preparation.phase.${view?.phase ?? item.state}`;
  return <>{t(key, { defaultValue: t("preparation.phase.waiting") })}</>;
}

// A failed copy is an operator decision: nothing retries it on its own, and
// the section says so rather than leaving the deployment looking stuck. The
// wording is shared by the drawer and the list row.
export function RetryHint() {
  const { t } = useTranslation("deployments");
  return <Typography.Text type="secondary">{t("preparation.noAutoRetry")}</Typography.Text>;
}

function isDownloading(item: ModelNodePresence) {
  const phase = item.preparation?.phase ?? item.state;
  return phase === "downloading" || (phase === "waiting" && item.state === "downloading" &&
    item.acquisition === "download" && item.rate_bps > 0);
}

function transferStatus(item: ModelNodePresence): StartupStatus {
  const reason = item.preparation?.reason;
  if (["failed", "canceled", "insufficient_disk_space"].includes(item.state) ||
    ["MODEL_PREPARATION_FAILED", "MODEL_CACHE_DISK_FULL", "MODEL_CACHE_EVICTION_FAILED"].includes(reason ?? "")) return "failed";
  if (item.state === "download_required" || (reason && reason !== "MODEL_PREPARATION_WAITING")) return "blocked";
  if (item.preparation ? item.preparation.phase === "waiting" : !["downloading", "syncing"].includes(item.state)) return "waiting";
  return "active";
}

export function DeploymentPreparation({ dep, rows, onChanged }: { dep?: Deployment; rows?: ModelNodePresence[]; onChanged?: () => void }) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const [busy, setBusy] = useState<string | null>(null);
  const status = dep?.placement_status;
  const reason = status?.reason;
  const imageWait = status?.unscheduled && reason?.code === "image_delivery_waiting"
    && typeof reason.params?.image === "string" && typeof reason.params?.node === "string"
    ? { imageRef: reason.params.image, node: reason.params.node,
      nodeUUID: typeof reason.params.node_uuid === "string" ? reason.params.node_uuid : undefined } : null;
  const owner = `${dep?.id}:${dep?.image}:${dep?.node_uuid}`;
  const [lastImage, setLastImage] = useState<{ owner: string; target: NonNullable<typeof imageWait> } | null>(null);
  const retainCompleted = (rows ?? []).some(item => !isModelFileReady(item.state)) || Boolean(imageWait)
    || Boolean(dep?.placement_status?.unscheduled)
    || Boolean(dep && dep.state === "pending" && !dep.instances?.length);
  const imageRef = imageWait?.imageRef, imageNode = imageWait?.node, imageNodeUUID = imageWait?.nodeUUID;
  useEffect(() => {
    if (imageRef && imageNode) setLastImage({ owner, target: { imageRef, node: imageNode, nodeUUID: imageNodeUUID } });
    else if (!retainCompleted) setLastImage(null);
  }, [owner, imageRef, imageNode, imageNodeUUID, retainCompleted]);
  const imageTarget = imageWait ?? (retainCompleted && lastImage?.owner === owner ? lastImage.target : null);
  const retry = async (item: ModelNodePresence) => {
    if (!item.model_id || !item.storage_id) return;
    setBusy(`${item.node_uuid}:${item.model_id}:${item.storage_id}`);
    try { await api.retryModelCache(item.model_id, item.node_uuid, item.storage_id); onChanged?.(); }
    catch (error) { message.error(apiErrorText(tCommon, error)); }
    finally { setBusy(null); }
  };
  const sharedDownloads = new Set<string>();
  const visible = (rows ?? []).filter(item => {
    const ready = isModelFileReady(item.state);
    if (ready && !retainCompleted) return false;
    const downloading = isDownloading(item);
    const shared = ["nfs", "nfs_cache", "shared_dir"].includes(item.storage_type ?? "");
    if (!shared || (!downloading && !ready) || !item.storage_id || !item.model_id) return true;
    const key = `${item.storage_id}:${item.model_id}:${item.transfer_id ?? ""}`;
    if (sharedDownloads.has(key)) return false;
    sharedDownloads.add(key);
    return true;
  });
  if (!visible.length && !imageTarget) return null;
  return <DetailSection title={t("detail.resourcePreparation")}>
    {visible.map((item) => {
      const key = `${item.node_uuid}:${item.model_id}:${item.storage_id}`;
      const view = item.preparation;
      const ready = isModelFileReady(item.state);
      const phase = view?.phase ?? item.state;
      const downloading = !ready && isDownloading(item);
      const compact = !ready && (phase === "syncing" || phase === "copying");
      const measurable = (view?.total_known ?? item.size_bytes > 0) && item.size_bytes > 0;
      const percent = Math.min(100, Math.max(0, Math.round(item.downloaded / item.size_bytes * 100)));
      const transmitted = item.transferred_bytes || item.downloaded || 0;
      const sharedDownload = downloading && ["nfs", "nfs_cache", "shared_dir"].includes(item.storage_type ?? "");
      const origin = transferOrigin(item);
      const eta = transferEtaSeconds(item);
      // A failure is the operator's to act on; its message is the agent's own
      // words about why, so it is never hidden behind a source line.
      const failed = view?.reason === "MODEL_PREPARATION_FAILED";
      const diskFull = view?.reason === "MODEL_CACHE_DISK_FULL";
      return <div key={key} className="mt-2 first:mt-0">
        <Space wrap>
          <StartupStatusIcon status={ready ? "done" : transferStatus(item)} />
          <Typography.Text strong>{sharedDownload ? item.storage_name || t("preparation.storage") : item.node_name || item.node_uuid.slice(0, 12)}</Typography.Text>
          {item.model_name && <Typography.Text>{item.model_name}</Typography.Text>}
          <Typography.Text type={ready ? "success" : view?.reason && view.reason !== "MODEL_PREPARATION_WAITING" ? "warning" : "secondary"}><PreparationLabel item={item} /></Typography.Text>
          {ready && item.size_bytes > 0 && <span className="text-fg-muted">{formatBytes(item.size_bytes)}</span>}
          {compact && <Typography.Text type="secondary">{measurable ? `${percent}%` : t("detailLayout.transferred", { size: formatBytes(transmitted) })}</Typography.Text>}
          {!sharedDownload && item.storage_id && <Link to={storageDetailPath(item.storage_id)}>{item.storage_name || t("preparation.storage")}</Link>}
        </Space>
        {downloading && measurable && <Progress aria-label={t("detailLayout.modelDownload")} percent={percent} size="small" />}
        {downloading && <div><Typography.Text type="secondary">
          {origin ? `${"node" in origin ? t("detail.modelFromNode", { node: origin.node }) : t("detail.modelFromHost", { host: origin.host })} · ` : ""}
          {measurable ? `${formatBytes(item.downloaded)} / ${formatBytes(item.size_bytes)}` : `${formatBytes(item.downloaded)} · ${t("preparation.unknownTotal")}`}
          {item.rate_bps > 0 ? ` · ${rateLabel(item.rate_bps)}` : ""}
          {measurable && eta > 0 ? ` · ${remainingText(t, eta)}` : ""}
        </Typography.Text></div>}
        {!ready && view?.retryable && <Space wrap>
          <Button aria-label={t("preparation.retry")} size="small" loading={busy === `${item.node_uuid}:${item.model_id}:${item.storage_id}`} disabled={busy !== null} onClick={() => void retry(item)}>{t("preparation.retry")}</Button>
          <RetryHint />
        </Space>}
        {!ready && failed && !view?.retryable && <Typography.Text type="secondary">{t("preparation.retryUnavailable")}</Typography.Text>}
        {!ready && item.message && ((!compact && !origin) || failed || diskFull) && <Typography.Paragraph type="secondary" style={{ marginBottom: 0 }}>{item.message}</Typography.Paragraph>}
        {!ready && view?.reason === "MODEL_COPY_STALLED" && <Typography.Paragraph className="flex items-center gap-1">{t("preparation.idle", { seconds: view.idle_seconds })} · {t("preparation.waitForIO")}<HelpTip content={t("preparation.waitForIOHelp")} /></Typography.Paragraph>}
      </div>;
    })}
    {imageTarget && <div className="mt-2 first:mt-0"><DeploymentImageProgress
      key={`${owner}:${imageTarget.imageRef}:${imageTarget.nodeUUID ?? imageTarget.node}`} {...imageTarget} /></div>}
  </DetailSection>;
}

// ListPreparationAction is the same retry, where the operator reads the failed
// state: the deployment list row. It is offered only on the server's own
// retryable fact and with the row's own storage, and a failure the server does
// not offer to retry says why instead of staying silent. The row is narrow, so
// the wording lives in the tooltip and the drawer says it in full.
export function ListPreparationAction({ item, onChanged }: { item: ModelNodePresence; onChanged?: () => void }) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const [busy, setBusy] = useState(false);
  const view = item.preparation;
  if (view?.retryable && item.model_id && item.storage_id) {
    const retry = async () => {
      setBusy(true);
      try { await api.retryModelCache(item.model_id!, item.node_uuid, item.storage_id!); onChanged?.(); }
      catch (error) { message.error(apiErrorText(tCommon, error)); }
      finally { setBusy(false); }
    };
    return <Tooltip title={t("preparation.noAutoRetry")}>
      <Button size="small" type="link" loading={busy} aria-label={t("preparation.retry")} onClick={() => void retry()}>{t("preparation.retry")}</Button>
    </Tooltip>;
  }
  if (view?.reason !== "MODEL_PREPARATION_FAILED") return null;
  return <Tooltip title={t("preparation.retryUnavailable")}><Typography.Text type="secondary" style={{ fontSize: "var(--font-size-label)" }}>{t("preparation.retryUnavailableShort")}</Typography.Text></Tooltip>;
}

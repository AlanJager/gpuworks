import { Button, Progress, Space, Typography, message } from "antd";
import { storageDetailPath } from "./storage/storageLinks";
import { useState } from "react";
import { apiErrorText } from "../apiError";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { api, type ModelNodePresence } from "../api";
import { formatBytes } from "../format";
import { isModelFileReady } from "../storageTypes";
import { rateLabel, transferOrigin } from "./models/modelPresenceDisplay";

// All classification, timing and what may be retried comes from the server's
// preparation view; the server also zeroes a rate that is not meaningful.
// Unknown stages and old agents deliberately have no synthetic percentage.
export function PreparationLabel({ item }: { item: ModelNodePresence }) {
  const { t } = useTranslation("deployments");
  const view = item.preparation;
  const key = view?.reason ? `preparation.reason.${view.reason}` : `preparation.phase.${view?.phase ?? "waiting"}`;
  return <>{t(key, { defaultValue: t("preparation.phase.waiting") })}</>;
}

export function DeploymentPreparation({ rows, onChanged }: { rows?: ModelNodePresence[]; onChanged?: () => void }) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const [busy, setBusy] = useState<string | null>(null);
  const retry = async (item: ModelNodePresence) => {
    if (!item.model_id || !item.storage_id) return;
    setBusy(`${item.node_uuid}:${item.model_id}:${item.storage_id}`);
    try { await api.retryModelCache(item.model_id, item.node_uuid, item.storage_id); onChanged?.(); }
    catch (error) { message.error(apiErrorText(tCommon, error)); }
    finally { setBusy(null); }
  };
  const pending = rows?.filter((item) => !isModelFileReady(item.state)) ?? [];
  if (pending.length === 0) return null;
  return <section style={{ marginTop: 16 }} aria-label={t("detail.modelPreparation")}>
    <Typography.Title level={5}>{t("detail.modelPreparation")}</Typography.Title>
    {pending.map((item) => {
      const view = item.preparation;
      const measurable = Boolean(view?.total_known) && item.size_bytes > 0;
      const origin = transferOrigin(item);
      return <div key={`${item.node_uuid}:${item.model_id}:${item.storage_id}`} style={{ marginTop: 12 }}>
        <Space wrap>
          <Typography.Text strong>{item.node_name || item.node_uuid.slice(0, 12)}</Typography.Text>
          {item.model_name && <Typography.Text>{item.model_name}</Typography.Text>}
          <Typography.Text type={view?.reason && view.reason !== "MODEL_PREPARATION_WAITING" ? "warning" : "secondary"}><PreparationLabel item={item} /></Typography.Text>
          {item.storage_id && <Link to={storageDetailPath(item.storage_id)}>{item.storage_name || t("preparation.storage")}</Link>}
        </Space>
        {measurable && <Progress aria-label={t("preparation.progress")} percent={Math.min(100, Math.round(item.progress * 100))} size="small" />}
        <div><Typography.Text type="secondary">
          {origin ? `${"node" in origin ? t("detail.modelFromNode", { node: origin.node }) : t("detail.modelFromHost", { host: origin.host })} · ` : ""}
          {measurable ? `${formatBytes(item.downloaded)} / ${formatBytes(item.size_bytes)}` : t("preparation.unknownTotal")}
          {item.rate_bps > 0 ? ` · ${rateLabel(item.rate_bps)}` : ""}
          {view?.eta_seconds ? ` · ${t("preparation.eta", { seconds: view.eta_seconds })}` : ""}
          {view?.reason === "MODEL_COPY_STALLED" ? ` · ${t("preparation.idle", { seconds: view.idle_seconds })}` : ""}
        </Typography.Text></div>
        {view?.retryable && <Button aria-label={t("preparation.retry")} size="small" loading={busy === `${item.node_uuid}:${item.model_id}:${item.storage_id}`} disabled={busy !== null} onClick={() => void retry(item)}>{t("preparation.retry")}</Button>}
        {item.message && !origin && <Typography.Paragraph type="secondary" style={{ marginBottom: 0 }}>{item.message}</Typography.Paragraph>}
        {view?.reason === "MODEL_COPY_STALLED" && <Typography.Paragraph>{t("preparation.waitForIO")}</Typography.Paragraph>}
      </div>;
    })}
  </section>;
}

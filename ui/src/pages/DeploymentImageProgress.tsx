import { useRef } from "react";
import { useTranslation } from "react-i18next";
import { api, usePolling, type ImageCatalogEntry } from "../api";
import { apiErrorText, productMessageText } from "../apiError";
import { Button } from "../components/ui/button";
import { formatBytes } from "../format";
import { Progress } from "./DeploymentControls";
import StartupStatusIcon from "./StartupStatusIcon";

export default function DeploymentImageProgress({ imageRef, node, nodeUUID }: {
  imageRef: string; node: string; nodeUUID?: string;
}) {
  const { t } = useTranslation("deployments");
  const { t: common } = useTranslation("common");
  const version = useRef<ImageCatalogEntry | null>(null);
  const { data, error, loading, refetch } = usePolling(async () => {
    if (!version.current) {
      const catalog = await api.imageVersions();
      version.current = catalog.items.find(item => item.image_ref === imageRef) ?? null;
    }
    if (!version.current?.series_id) return null;
    const series = await api.image(version.current.series_id);
    const rows = series.versions?.find(item => item.id === version.current?.id)?.nodes ?? [];
    const matches = rows.filter(row => nodeUUID ? row.node_uuid === nodeUUID : row.node_name === node || row.node_uuid === node);
    return matches.length === 1 ? matches[0] : null;
  }, 2000);
  const active = data?.state === "pulling" || data?.state === "syncing";
  const hasTransferred = typeof data?.transferred_bytes === "number" && Number.isFinite(data.transferred_bytes);
  const done = Math.max(0, data?.transferred_bytes ?? 0);
  const total = Math.max(0, data?.size_bytes ?? 0);
  const measurable = active && hasTransferred && total > 0;
  const percent = measurable ? Math.min(100, Math.floor(done / total * 100)) : undefined;
  const rate = active && (total === 0 || done < total) ? Math.max(0, data?.rate_bps ?? 0) : 0;
  const phase = error ? "unavailable" : !data ? loading ? "loading" : "waiting"
    : data.state === "failed" && data.transfer_error_code === "PEER_TRANSFER_CANCELED" ? "canceled"
    : ["pulling", "syncing", "present", "failed"].includes(data.state) ? data.state : "waiting";
  const tone = phase === "failed" || error ? "text-fg-danger" : phase === "present" ? "text-fg-success" : "text-fg-subtle";
  const failure = data?.transfer_error_code
    ? productMessageText(common, { code: data.transfer_error_code, params: { reason: data.message ?? "" }, message: data.message ?? "" })
    : data?.message;
  return <div className="min-w-0" aria-label={t("imagePreparation.label")}>
    <div className="inline-flex flex-wrap items-center gap-2 text-fg-muted">
      <div><StartupStatusIcon status={phase === "failed" || error ? "failed" : phase === "present" ? "done" : active ? "active" : "waiting"} /></div>
      <div><strong className="text-body font-semibold">{node}</strong></div>
      <div className="min-w-0"><span className="break-all text-body">{imageRef}</span></div>
      <div><span className={`text-body ${tone}`}>{t(`imagePreparation.${phase}`)}</span></div>
      {phase === "present" && total > 0 && <div><span className="text-fg-muted tabular-nums">{formatBytes(total)}</span></div>}
    </div>
    {!error && <>
      {measurable && <Progress percent={percent} aria-label={t("imagePreparation.progress", { node })} />}
      {active && !measurable && <div className="deployment-progress">
        <div className="instance-progress-unknown" role="progressbar" aria-label={t("imagePreparation.progress", { node })}
          aria-valuetext={t(`imagePreparation.${phase}`)}><span /></div>
      </div>}
      {phase !== "present" && hasTransferred && (active || done > 0) && <div><span className="text-body text-fg-subtle tabular-nums">
        {total > 0 ? `${formatBytes(done)} / ${formatBytes(total)}` : t("imagePreparation.transferred", { size: formatBytes(done) })}
        {rate > 0 && ` · ${formatBytes(rate)}/s`}
      </span></div>}
      {(phase === "failed" || phase === "canceled") && failure && <div className="break-words text-fg-danger">{failure}</div>}
    </>}
    {error && <div className="flex flex-wrap items-center gap-2">
      <span className="break-words">{apiErrorText(common, error)}</span>
      <Button size="sm" variant="tertiary" loading={loading} onClick={() => void refetch()}>{t("imagePreparation.reload")}</Button>
    </div>}
  </div>;
}

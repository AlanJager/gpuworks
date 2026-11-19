import { Badge } from "./ui/badge";
import { Notice } from "../ui-kit/Notice";
import { useTranslation } from "react-i18next";
import type { PartitionCapability as Capability } from "../api";
import { formatTime } from "../i18n/datetime";
import { partitionReasonText } from "./partitionCapabilities";

export default function PartitionCapability({ capability, compact = false }: { capability?: Capability; compact?: boolean }) {
  const { t } = useTranslation("common");
  if (!capability) return <span className="text-fg-muted">{t("partitionCapability.none")}</span>;
  const ready = capability.status === "ready";
  const reason = partitionReasonText(t, capability);
  const probeDetail = capability.reason_params?.probe_detail;
  const probeStatusText = capability.probe_status
    ? t(`partitionCapability.probeStatus.${capability.probe_status}`, { defaultValue: capability.probe_status })
    : undefined;
  if (compact) {
    return <span className="inline-flex flex-wrap items-center gap-1">
      <Badge color={ready ? "green" : "gray"}>{capability.display_name || capability.backend}</Badge>
      <Badge status={ready ? "success" : "danger"}>{ready ? t("partitionCapability.available") : t("partitionCapability.unavailable")}</Badge>
      {capability.maturity === "experimental" && <Badge color="orange">{t("partitionCapability.experimental")}</Badge>}
      {!!capability.compatibility_notes?.length && <Badge color="amber">{t("partitionCapability.compatibilityHint")}</Badge>}
    </span>;
  }
  // A ready backend is plain (its badge says ready); an experimental one warns.
  return <Notice
    tone={ready ? (capability.maturity === "experimental" ? "warning" : "default") : "info"}
    role="alert"
    title={<span className="inline-flex flex-wrap items-center gap-2">
      <span className="font-semibold">{capability.display_name || capability.backend}</span>
      <Badge status={ready ? "success" : "danger"}>{ready ? t("partitionCapability.ready") : t("partitionCapability.notReady")}</Badge>
      {capability.maturity === "experimental" && <Badge color="orange">{t("partitionCapability.experimentalBackend")}</Badge>}
      {capability.probe_status && <Badge color={capability.probe_status === "passed" ? "green" : capability.probe_status === "failed" ? "red" : "gray"}>{t("partitionCapability.probe", { status: probeStatusText })}</Badge>}
    </span>}
  >
    <div className="flex flex-col gap-0.5">
      {capability.compatibility_summary && <span>{capability.compatibility_summary}</span>}
      {capability.probe_checked_at && <span className="text-fg-muted">{t("partitionCapability.lastChecked", { time: formatTime(capability.probe_checked_at) })}</span>}
      {!!capability.compatibility_notes?.length && <Notice tone="warning" role="alert" title={t("partitionCapability.compatibilityHint")}><div className="flex flex-col">{capability.compatibility_notes.map((note) => <span key={note}>{note}</span>)}</div></Notice>}
      {reason && <span>{reason}{probeDetail && <> <code>{probeDetail}</code></>}</span>}
    </div>
  </Notice>;
}

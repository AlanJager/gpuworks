import DetailSection from "./detail/DetailSection";
import type { Breakdown, Measured } from "../api";
import { Badge, type BadgeColor } from "./ui/badge";
import { colors } from "../theme/tokens";
import { formatVRAMFitSummary } from "../pages/deploy/vramDisplay";
import { useTranslation } from "react-i18next";

// Horizontal stacked memory bar for a single GPU's estimated VRAM usage, with an
// optional measured bar beneath for estimate-vs-actual comparison.

const GIB = 1024 ** 3;

// The text colour for each tone the fit summary and concurrency line report.
const textTone = { success: "text-fg-success", danger: "text-fg-danger", secondary: "text-fg-muted" } as const;
const toGiB = (bytes: number) => bytes / GIB;

const COLORS = {
  weights: colors.chartPrimary,
  kv: colors.chartTertiary,
	mm: colors.chartQuaternary,
  reserve: colors.chartViolet,
  // Only the measured bar has a graph segment: the engine reports its CUDA graph
  // capture, while the estimate deliberately models no such thing.
  graph: colors.chartSecondary,
} as const;

interface Seg {
  key: string;
  label: string;
  color: string;
  gib: number;
}

export default function VRAMBreakdown({
  breakdown,
  measured,
  inDetailCard = false,
}: {
  inDetailCard?: boolean;
  breakdown: Breakdown;
  measured?: Measured;
}) {
  const { t } = useTranslation("deploy");
  const gradeMap: Record<string, { text: string; color: BadgeColor }> = {
    measured: { text: t("vram.grade.measured"), color: "green" },
    rough_upper: { text: t("vram.grade.rough"), color: "orange" },
    // A model that keeps no KV cache is answered, not unanswered.
    not_applicable: { text: t("vram.grade.notApplicable"), color: "gray" },
  };
  const tp = breakdown.tensor_parallel > 0 ? breakdown.tensor_parallel : 1;
  const fixedSegs: Seg[] = [
    { key: "weights", label: t("vram.segment.weights"), color: COLORS.weights, gib: toGiB(breakdown.weight_bytes) / tp },
		{ key: "mm", label: t("vram.segment.multimodal"), color: COLORS.mm, gib: toGiB(breakdown.mm_encoder_bytes || 0) / tp },
    { key: "reserve", label: t("vram.segment.reserve"), color: COLORS.reserve, gib: toGiB(breakdown.reserve_bytes || 0) },
  ];
	const estSegs: Seg[] = [...fixedSegs,
	  { key: "kv", label: t("vram.segment.kvPool"), color: COLORS.kv, gib: toGiB(breakdown.kv_pool_bytes || 0) },
	];
	const estTotal = toGiB(breakdown.per_gpu_bytes);
	const startupMinimumGiB = toGiB(breakdown.startup_fixed_bytes ?? breakdown.per_gpu_bytes);
	const kvPoolGiB = toGiB(breakdown.kv_pool_bytes || 0);
	const displayEstimateTotal = estTotal + kvPoolGiB;
	const capacityGiB = breakdown.capacity_bytes ? toGiB(breakdown.capacity_bytes) : undefined;
  // Denominator (GiB) that maps to 100% bar width — capacity if known, else total.
  const denom = capacityGiB && capacityGiB > estTotal ? capacityGiB : estTotal || 1;

	const confidenceKey = breakdown.kv_estimate_confidence || breakdown.grade;
  const grade = gradeMap[confidenceKey] ?? { text: confidenceKey || "—", color: "gray" as BadgeColor };
	const fitSummary = formatVRAMFitSummary(breakdown, t);
	const showEstimated = breakdown.source !== "measured";

  const hasMeasured = !!measured && measured.Found;
	const measSegs: Seg[] = hasMeasured
    ? [
        { key: "weights", label: t("vram.segment.weights"), color: COLORS.weights, gib: measured!.ModelWeightsGiB },
        { key: "kv", label: "KV cache", color: COLORS.kv, gib: measured!.KVCacheGiB },
        { key: "graph", label: t("vram.segment.graph"), color: COLORS.graph, gib: measured!.GraphCaptureGiB },
      ]
    : [];
  const measuredTotalGiB = measSegs.reduce((sum, segment) => sum + segment.gib, 0);
  const measuredOnly = hasMeasured && !showEstimated;
  const measuredKvSummary = hasMeasured && measured.KVCacheTokens > 0
    ? t("vram.measuredKvSummary", { value: measured.KVCacheGiB.toFixed(1), tokens: measured.KVCacheTokens.toLocaleString() })
    : undefined;
  function allocationTag(label: string, calibrated = false) {
    return measuredOnly
      ? <Badge size="sm" status={calibrated ? "success" : undefined}>{label}</Badge>
      : <Badge color={calibrated ? "green" : "gray"}>{label}</Badge>;
  }
  const metadata = (
    <>
      {breakdown.source === "measured" && allocationTag(t("vram.measuredCalibration"), true)}
      {showEstimated && <Badge color={grade.color}>{t("vram.confidence", { value: grade.text })}</Badge>}
      {breakdown.tensor_parallel > 1 && allocationTag(`tp=${breakdown.tensor_parallel}`)}
      {!!breakdown.vram_limit_bytes && breakdown.vram_limit_bytes > 0 && allocationTag(t("vram.partitionCapacity", { value: toGiB(breakdown.vram_limit_bytes).toFixed(1) }))}
      {breakdown.max_model_len > 0 && (
        <span className="text-fg-muted text-label">
          {t("vram.context", { value: breakdown.max_model_len })}
        </span>
      )}
    </>
  );

  const body = (
    <>
      {showEstimated && <span className={`block mb-2 font-semibold ${textTone[fitSummary.tone]}`}>
		  {fitSummary.text}
		</span>}
      {showEstimated && typeof breakdown.max_concurrency === "number" && (
        <span className={`block mb-2 ${textTone[breakdown.max_concurrency < 1 ? "danger" : "secondary"]}`}>
          {t("vram.maxConcurrency", { value: breakdown.max_concurrency })}
        </span>
      )}

      {showEstimated && <Bar label={t("vram.estimated")} segs={estSegs} denom={denom} total={displayEstimateTotal} capacity={capacityGiB} />}
      {hasMeasured && (
        <Bar
          label={t("vram.measured")}
          segs={measSegs}
          denom={denom}
          total={measuredTotalGiB}
          capacity={capacityGiB}
          compact={measuredOnly}
          note={measuredOnly ? measuredKvSummary : undefined}
        />
      )}

      {showEstimated && <div style={{ marginTop: 10 }}>
        {estSegs
          .filter((s) => s.gib > 0.001)
          .map((s) => (
            <span
              key={s.key}
              style={{ display: "inline-flex", alignItems: "center", gap: 6, marginRight: 16, marginBottom: 4 }}
            >
              <span
                style={{ width: 12, height: 12, borderRadius: "var(--radius-sm)", background: s.color, display: "inline-block" }}
              />
              <span className="text-label">
                {s.label} {s.gib.toFixed(1)} GiB
              </span>
            </span>
          ))}
      </div>}
      {!measuredOnly && measuredKvSummary && (
        <span className="text-fg-muted block mt-1.5">
          {measuredKvSummary}
        </span>
      )}
      {showEstimated && (breakdown.kv_pool_bytes != null || breakdown.estimated_kv_tokens != null) && (
        <span className="text-fg-muted block mt-1.5">
          {breakdown.estimated_kv_tokens != null && breakdown.estimated_kv_tokens > 0
            ? t("vram.kvPoolSummary", { value: toGiB(breakdown.kv_pool_bytes || 0).toFixed(1), tokens: breakdown.estimated_kv_tokens.toLocaleString() })
            : breakdown.kv_estimate_confidence === "not_applicable"
              ? t("vram.kvPoolNotApplicable", { value: toGiB(breakdown.kv_pool_bytes || 0).toFixed(1) })
              : t("vram.kvPoolUnavailable", {
                  value: toGiB(breakdown.kv_pool_bytes || 0).toFixed(1),
                  // The server names the reason as a stable code and the locale
                  // lives here, so an unknown code falls back to a phrase in the
                  // reader's language rather than to the server's English.
                  reason: t(`vram.kvReason.${breakdown.kv_estimate_reason}`, {
                    defaultValue: t("vram.kvReason.unknown"),
                  }),
                })}
        </span>
      )}
      {showEstimated && <span className="text-fg-muted block mt-1">{t("vram.startupMinimum", { value: startupMinimumGiB.toFixed(1) })}</span>}
    </>
  );
  if (inDetailCard) return (
    <DetailSection title={t("vram.title")} controls={<div className="flex flex-wrap items-center gap-x-2 gap-y-0.5">{metadata}</div>}>
      {body}
    </DetailSection>
  );
  return <div>
    <div className={measuredOnly ? "mb-[3px] flex flex-wrap items-center gap-x-2 gap-y-0.5" : "mb-2 flex flex-wrap items-center gap-4"}>
      <span className="font-semibold">{t("vram.title")}</span>{metadata}
    </div>
    {body}
  </div>;
}

function Bar({
  label,
  segs,
  denom,
  total,
  capacity,
  compact = false,
  note,
}: {
  label: string;
  segs: Seg[];
  denom: number;
  total: number;
  capacity?: number;
  compact?: boolean;
  note?: string;
}) {
  const { t } = useTranslation("deploy");
  const util = capacity ? (total / capacity) * 100 : undefined;
  const amount = capacity ? `${total.toFixed(1)} / ${capacity.toFixed(1)}` : total.toFixed(1);
  return (
    <div style={{ marginBottom: compact ? 0 : 8 }}>
      {!compact && <div style={{ display: "flex", flexWrap: "wrap", alignItems: "center", justifyContent: "space-between", gap: "2px 8px", marginBottom: 3 }}>
        <span className="text-fg-muted text-label">{label}</span>
        <span className="text-fg-muted text-label">
          {amount} GiB
          {util != null && t("vram.utilization", { value: util.toFixed(0) })}
        </span>
      </div>}
      <div
        className={compact ? "flex h-2 w-full overflow-hidden rounded-full bg-emphasis" : undefined}
        style={compact ? undefined : {
          display: "flex",
          height: 26,
          borderRadius: "var(--radius-md)",
          overflow: "hidden",
          background: colors.borderSoft,
          border: `1px solid ${colors.border}`,
        }}
      >
        {segs
          .filter((s) => s.gib > 0.001)
          .map((s) => (
            <div
              key={s.key}
              title={`${s.label} ${s.gib.toFixed(2)} GiB`}
              style={{ width: `${(s.gib / denom) * 100}%`, background: s.color }}
            />
          ))}
      </div>
      {compact && <div style={{ display: "flex", flexWrap: "wrap", gap: "2px 12px", marginTop: 4, fontSize: "var(--font-size-label)", color: colors.muted }}>
        <span>
          {t("vram.actualTotal", { value: amount })}
          {util != null && t("vram.utilization", { value: util.toFixed(0) })}
        </span>
        {note && <span>{note}</span>}
      </div>}
    </div>
  );
}

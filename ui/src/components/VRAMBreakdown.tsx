import { Space, Tag, Typography } from "antd";
import type { Breakdown, Measured } from "../api";
import { colors } from "../theme/tokens";
import { formatVRAMFitSummary } from "../pages/deploy/vramDisplay";
import { useTranslation } from "react-i18next";

// Horizontal stacked memory bar for a single GPU's estimated VRAM usage, with an
// optional measured bar beneath for estimate-vs-actual comparison.

const GIB = 1024 ** 3;
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
}: {
  breakdown: Breakdown;
  measured?: Measured;
}) {
  const { t } = useTranslation("deploy");
  const gradeMap: Record<string, { text: string; color: string }> = {
    measured: { text: t("vram.grade.measured"), color: "green" },
    rough_upper: { text: t("vram.grade.rough"), color: "orange" },
    // A model that keeps no KV cache is answered, not unanswered.
    not_applicable: { text: t("vram.grade.notApplicable"), color: "default" },
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
  const grade = gradeMap[confidenceKey] ?? { text: confidenceKey || "—", color: "default" };
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

  return (
    <div>
      <Space wrap size="middle" style={{ marginBottom: 8 }}>
        <Typography.Text strong>{t("vram.title")}</Typography.Text>
        {breakdown.source === "measured" && <Tag color="green">{t("vram.measuredCalibration")}</Tag>}
        {showEstimated && <Tag color={grade.color}>{t("vram.confidence", { value: grade.text })}</Tag>}
        {breakdown.tensor_parallel > 1 && <Tag>tp={breakdown.tensor_parallel}</Tag>}
		{breakdown.vram_limit_bytes && breakdown.vram_limit_bytes > 0 && <Tag>{t("vram.partitionCapacity", { value: toGiB(breakdown.vram_limit_bytes).toFixed(1) })}</Tag>}
        {breakdown.max_model_len > 0 && (
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {t("vram.context", { value: breakdown.max_model_len })}
          </Typography.Text>
        )}
      </Space>

      {showEstimated && <Typography.Text type={fitSummary.tone} strong style={{ display: "block", marginBottom: 8 }}>
		  {fitSummary.text}
		</Typography.Text>}
      {showEstimated && typeof breakdown.max_concurrency === "number" && (
        <Typography.Text type={breakdown.max_concurrency < 1 ? "danger" : "secondary"} style={{ display: "block", marginBottom: 8 }}>
          {t("vram.maxConcurrency", { value: breakdown.max_concurrency })}
        </Typography.Text>
      )}

      {showEstimated && <Bar label={t("vram.estimated")} segs={estSegs} denom={denom} total={displayEstimateTotal} capacity={capacityGiB} utilizationLabel={(value) => t("vram.utilization", { value })} />}
      {hasMeasured && (
        <Bar
          label={t("vram.measured")}
          segs={measSegs}
          denom={denom}
          total={measSegs.reduce((s, x) => s + x.gib, 0)}
          capacity={capacityGiB}
          utilizationLabel={(value) => t("vram.utilization", { value })}
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
                style={{ width: 12, height: 12, borderRadius: 3, background: s.color, display: "inline-block" }}
              />
              <Typography.Text style={{ fontSize: 12 }}>
                {s.label} {s.gib.toFixed(1)} GiB
              </Typography.Text>
            </span>
          ))}
      </div>}
      {hasMeasured && measured!.KVCacheTokens > 0 && (
        <Typography.Text type="secondary" style={{ display: "block", marginTop: 6 }}>
          {t("vram.measuredKvSummary", {
            value: measured!.KVCacheGiB.toFixed(1),
            tokens: measured!.KVCacheTokens.toLocaleString(),
          })}
        </Typography.Text>
      )}
      {showEstimated && (breakdown.kv_pool_bytes != null || breakdown.estimated_kv_tokens != null) && (
        <Typography.Text type="secondary" style={{ display: "block", marginTop: 6 }}>
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
        </Typography.Text>
      )}
      {showEstimated
        ? <Typography.Text type="secondary" style={{ display: "block", marginTop: 4 }}>{t("vram.startupMinimum", { value: startupMinimumGiB.toFixed(1) })}</Typography.Text>
        : hasMeasured && <Typography.Text type="secondary" style={{ display: "block", marginTop: 4 }}>{t("vram.actualTotal", { value: measuredTotalGiB.toFixed(1) })}</Typography.Text>}
    </div>
  );
}

function Bar({
  label,
  segs,
  denom,
  total,
  capacity,
  utilizationLabel,
}: {
  label: string;
  segs: Seg[];
  denom: number;
  total: number;
  capacity?: number;
  utilizationLabel: (value: string) => string;
}) {
  const util = capacity ? (total / capacity) * 100 : undefined;
  return (
    <div style={{ marginBottom: 8 }}>
      <div style={{ display: "flex", justifyContent: "space-between", marginBottom: 3 }}>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {label}
        </Typography.Text>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {total.toFixed(1)}
          {capacity ? ` / ${capacity.toFixed(1)} GiB` : " GiB"}
          {util != null ? utilizationLabel(util.toFixed(0)) : ""}
        </Typography.Text>
      </div>
      <div
        style={{
          display: "flex",
          height: 26,
          borderRadius: 2,
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
    </div>
  );
}

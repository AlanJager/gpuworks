import { Tooltip, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { PhysicalGPU } from "../api";
import { colors } from "../theme/tokens";

const GIB = 1024 ** 3;
const SLICE_COLORS = [colors.chartPrimary, colors.chartTeal, colors.chartViolet, colors.chartQuaternary, colors.chartSecondary];

export interface MemorySegment {
  key: string;
  label: string;
  bytes: number;
  color: string;
  // A segment the platform counts against the card but no engine asked for:
  // drawn hatched so it does not read as somebody's slice.
  overhead?: boolean;
}

// memorySegments splits a card's memory the way the scheduler books it: each
// occupant's share (a dGPU slice's limit, or the whole card), what slices
// hold beyond their limits, a sleeper's residue, and what is left. The
// figures come from the inventory (allocated_bytes / available_bytes), so
// the bar can never disagree with the card's allocation state; occupant
// limits only say how the allocated part divides.
export function memorySegments(gpu: Pick<PhysicalGPU, "memory_total_bytes" | "allocated_bytes" | "available_bytes" | "occupants">, labels: {
  slice: (name: string, limitGiB: string) => string;
  wholeCard: (name: string) => string;
  overhead: string;
  sleeping: string;
  free: string;
}): MemorySegment[] {
  const total = Math.max(gpu.memory_total_bytes, 0);
  const allocated = Math.min(Math.max(gpu.allocated_bytes, 0), total);
  const available = Math.min(Math.max(gpu.available_bytes, 0), total - allocated);
  const awake = gpu.occupants.filter((item) => !item.sleep_level);
  const slices = awake.filter((item) => (item.limit_bytes ?? 0) > 0);
  const segments: MemorySegment[] = [];
  if (allocated > 0 && slices.length === awake.length && slices.length > 0) {
    let limits = 0;
    slices.forEach((item, index) => {
      const bytes = Math.min(item.limit_bytes ?? 0, allocated - limits);
      limits += bytes;
      segments.push({
        key: item.instance_uuid,
        label: labels.slice(item.deployment_name || item.instance_uuid.slice(0, 8), ((item.limit_bytes ?? 0) / GIB).toFixed(1)),
        bytes,
        color: SLICE_COLORS[index % SLICE_COLORS.length],
      });
    });
    if (allocated - limits > 0) segments.push({ key: "overhead", label: labels.overhead, bytes: allocated - limits, color: colors.chartTick, overhead: true });
  } else if (allocated > 0) {
    const holder = awake.find((item) => !item.limit_bytes) ?? awake[0];
    segments.push({ key: holder?.instance_uuid ?? "allocated", label: labels.wholeCard(holder ? holder.deployment_name || holder.instance_uuid.slice(0, 8) : "-"), bytes: allocated, color: colors.chartPrimary });
  }
  const sleeping = total - allocated - available;
  if (sleeping > 0) segments.push({ key: "sleeping", label: labels.sleeping, bytes: sleeping, color: colors.chartQuaternary, overhead: true });
  if (available > 0) segments.push({ key: "free", label: labels.free, bytes: available, color: colors.borderSoft });
  return segments;
}

// GPUMemoryComposition draws one card's memory as booked, with the memory
// the card reports in use marked on top: the two differ by design (a slice
// books its limit, its engine may use less), and seeing both is what tells
// an operator whether a refusal is the books or the card.
export default function GPUMemoryComposition({ gpu }: { gpu: PhysicalGPU }) {
  const { t } = useTranslation("gpus");
  const segments = memorySegments(gpu, {
    slice: (name, limit) => t("composition.slice", { name, limit }),
    wholeCard: (name) => t("composition.wholeCard", { name }),
    overhead: t("composition.overhead"),
    sleeping: t("composition.sleeping"),
    free: t("composition.free"),
  });
  const total = gpu.memory_total_bytes || 1;
  const used = gpu.load.memory_used_bytes;
  return (
    <div className="gpu-memory-composition">
      <div className="gpu-memory-composition-bar" role="img" aria-label={t("composition.aria")}>
        {segments.map((segment) => (
          <Tooltip key={segment.key} title={`${segment.label} · ${fmtGiB(segment.bytes)}`}>
            <span
              className={segment.overhead ? "is-overhead" : undefined}
              style={{ width: `${(segment.bytes / total) * 100}%`, background: segment.color }}
            />
          </Tooltip>
        ))}
        {used != null && (
          <Tooltip title={t("composition.used", { used: fmtGiB(used) })}>
            <i style={{ left: `${Math.min(100, (used / total) * 100)}%` }} />
          </Tooltip>
        )}
      </div>
      <ul className="gpu-memory-composition-legend">
        {segments.map((segment) => (
          <li key={segment.key}>
            <span className={`gpu-memory-swatch${segment.overhead ? " is-overhead" : ""}`} style={{ background: segment.color }} />
            <Typography.Text>{segment.label}</Typography.Text>
            <Typography.Text type="secondary">{fmtGiB(segment.bytes)}</Typography.Text>
          </li>
        ))}
        {used != null && (
          <li>
            <span className="gpu-memory-swatch is-marker" />
            <Typography.Text>{t("composition.usedLegend")}</Typography.Text>
            <Typography.Text type="secondary">{fmtGiB(used)}</Typography.Text>
          </li>
        )}
      </ul>
      <Typography.Text type="secondary" className="gpu-memory-composition-note">{t("composition.note")}</Typography.Text>
    </div>
  );
}

function fmtGiB(value: number) { return `${(value / GIB).toFixed(1)} GiB`; }

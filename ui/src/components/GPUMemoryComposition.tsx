import { Tooltip } from "./ui/tooltip";
import { useTranslation } from "react-i18next";
import type { PhysicalGPU } from "../api";
import { colors } from "../theme/tokens";
import { HelpTip } from "../ui-kit/HelpTip";

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
  // Why the segment is there, for the ⓘ beside its legend entry.
  help?: string;
}

// memorySegments splits a card's memory the way the scheduler books it: each
// occupant's share (a dGPU slice's limit, or the whole card), what slices
// hold beyond their limits, a sleeper's residue, what the platform withholds
// for another reason, and what is left. The figures come from the inventory
// (allocated_bytes / sleeping_bytes / foreign_bytes / available_bytes), so
// the bar can never disagree with the card's allocation state; occupant
// limits only say how the allocated part divides.
//
// What is withheld is named by its cause, never as a sleeper's. On a card
// processes outside the platform hold it is two things: their memory, a
// reading that matches the vendor tool, and the headroom the deployment
// policy keeps beside it, which nobody uses - none when the policy keeps
// none. And what is left is "allocatable" only on a card that takes work; on
// one that does not it is merely free.
export function memorySegments(gpu: Pick<PhysicalGPU, "memory_total_bytes" | "allocated_bytes" | "sleeping_bytes" | "foreign_bytes" | "available_bytes" | "occupants" | "foreign_processes" | "schedulable" | "allocation_state">, labels: {
  slice: (name: string, limitGiB: string) => string;
  wholeCard: (name: string) => string;
  overhead: string;
  sleeping: string;
  foreign: string;
  foreignUnknown: string;
  headroom: string;
  headroomHelp: string;
  withheld: string;
  free: string;
  idle: string;
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
  const sleeping = Math.min(Math.max(gpu.sleeping_bytes || 0, 0), total - allocated - available);
  if (sleeping > 0) segments.push({ key: "sleeping", label: labels.sleeping, bytes: sleeping, color: colors.chartQuaternary, overhead: true });
  const withheld = total - allocated - sleeping - available;
  // On a node nobody can reach nothing is on offer whatever holds the card,
  // so its outside processes do not account for what is withheld there.
  const held = gpu.allocation_state !== "unknown" && (gpu.foreign_processes?.length ?? 0) > 0;
  const foreign = Math.min(Math.max(gpu.foreign_bytes || 0, 0), withheld);
  if (withheld > 0 && !held) {
    segments.push({ key: "withheld", label: labels.withheld, bytes: withheld, color: colors.chartSlate, overhead: true });
  } else if (withheld > 0 && foreign <= 0) {
    // Held by processes whose memory is not known: the whole card is kept
    // from the platform, and no part of it can be called headroom.
    segments.push({ key: "foreign", label: labels.foreignUnknown, bytes: withheld, color: colors.chartSlate });
  } else if (withheld > 0) {
    segments.push({ key: "foreign", label: labels.foreign, bytes: foreign, color: colors.chartSlate });
    if (withheld - foreign > 0) segments.push({ key: "headroom", label: labels.headroom, help: labels.headroomHelp, bytes: withheld - foreign, color: colors.chartSlate, overhead: true });
  }
  if (available > 0) segments.push({ key: "free", label: gpu.schedulable ? labels.free : labels.idle, bytes: available, color: colors.borderSoft });
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
    foreign: t("composition.foreign"),
    foreignUnknown: t("composition.foreignUnknown"),
    headroom: t("composition.headroom"),
    headroomHelp: t("composition.headroomHelp"),
    withheld: t("composition.withheld"),
    free: t("composition.free"),
    idle: t("composition.idle"),
  });
  const total = gpu.memory_total_bytes || 1;
  const used = gpu.load.memory_used_bytes;
  return (
    <div className="gpu-memory-composition">
      <div className="gpu-memory-composition-bar" role="img" aria-label={t("composition.aria")}>
        {segments.map((segment) => (
          <Tooltip key={segment.key} content={`${segment.label} · ${fmtGiB(segment.bytes)}`}>
            <span
              className={segment.overhead ? "is-overhead" : undefined}
              // backgroundColor, not the shorthand: the shorthand would reset
              // the hatching .is-overhead draws as a background image.
              style={{ width: `${(segment.bytes / total) * 100}%`, backgroundColor: segment.color }}
            />
          </Tooltip>
        ))}
        {used != null && (
          <Tooltip content={t("composition.used", { used: fmtGiB(used) })}>
            <i style={{ left: `${Math.min(100, (used / total) * 100)}%` }} />
          </Tooltip>
        )}
      </div>
      <ul className="gpu-memory-composition-legend">
        {segments.map((segment) => (
          <li key={segment.key}>
            <span className={`gpu-memory-swatch${segment.overhead ? " is-overhead" : ""}`} style={{ backgroundColor: segment.color }} />
            <span className="min-w-0 truncate">{segment.label}</span>
            {segment.help && <HelpTip content={segment.help} label={t("composition.helpLabel", { name: segment.label })} />}
            <span className="ml-auto flex-none tabular-nums text-fg-muted">{fmtGiB(segment.bytes)}</span>
          </li>
        ))}
        {used != null && (
          <li>
            <span className="gpu-memory-swatch is-marker" />
            <span className="min-w-0 truncate">{t("composition.usedLegend")}</span>
            <span className="ml-auto flex-none tabular-nums text-fg-muted">{fmtGiB(used)}</span>
          </li>
        )}
      </ul>
    </div>
  );
}

function fmtGiB(value: number) { return `${(value / GIB).toFixed(1)} GiB`; }

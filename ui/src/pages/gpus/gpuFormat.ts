import type { PhysicalGPU } from "../../api";
import { cardOrdinal } from "../../nodes/cardIdentity";

export const GIB = 1024 ** 3;

export const allocationLabels: Record<PhysicalGPU["allocation_state"], { labelKey: string; color: string }> = {
  available: { labelKey: "allocation.available", color: "green" },
  partial: { labelKey: "allocation.partial", color: "blue" },
  allocated: { labelKey: "allocation.allocated", color: "gold" },
  unknown: { labelKey: "allocation.unknown", color: "default" },
  // A card that takes no work: its health, its ownership, or somebody
  // else's process on it. Which one is said beside it, by its own state.
  unusable: { labelKey: "allocation.unusable", color: "red" },
};

// "GPU 3" for a card with a device number; a card without one is named by
// its PCI address, never "GPU -1".
export function gpuHeading(gpu: Pick<PhysicalGPU, "index" | "pcie_bdf">) { return gpu.index >= 0 ? `GPU ${gpu.index}` : cardOrdinal(gpu); }
export { vendorName as vendorLabel } from "../../vendorName";
export function fmtGiB(value: number) { return `${(value / GIB).toFixed(value >= 10 * GIB ? 0 : 1)} GiB`; }
// Zero capacity means unknown; measured zero usage still uses fmtGiB.
export function fmtCapacityGiB(value: number) { return Number.isFinite(value) && value > 0 ? fmtGiB(value) : "—"; }
export function fmtRate(value?: number) { return value == null ? "-" : value >= GIB ? `${(value / GIB).toFixed(1)} GiB/s` : `${(value / 1024 ** 2).toFixed(1)} MiB/s`; }
export function optionalPercent(value?: number) { return value == null ? "-" : `${value.toFixed(0)}%`; }
export function dash(value?: string) { return value || "-"; }
export function shortID(value: string) { return value.length > 16 ? `${value.slice(0, 12)}...` : value; }

import type { TFunction } from "i18next";
import type { MonitorPlacement } from "../../api";

/**
 * Formats a service's placement facts for display: `TP4 · gpu3`, or
 * `4 nodes × 1 GPU · gb10-1, gb10-2, gb10-3, gb10-4` for a multi-node replica.
 * The parts are the server's facts only: TP shows when the server sent it and
 * is never derived here from the card or node count, and the node names keep
 * the server's rank order. `t` is the monitoring-namespace translator.
 */
export function formatPlacement(t: TFunction, placement: MonitorPlacement): string {
  const parts: string[] = [];
  if (placement.tensor_parallel !== undefined) {
    parts.push(t("placement.tensorParallel", { count: placement.tensor_parallel }));
  }
  if (placement.nnodes > 1) {
    parts.push(t("placement.multiNode", { nodes: placement.nnodes, count: placement.gpus_per_node }));
  } else if (placement.tensor_parallel === undefined && placement.gpus_per_node > 0) {
    parts.push(t("placement.gpus", { count: placement.gpus_per_node }));
  }
  if (placement.nodes.length > 0) parts.push(placement.nodes.join(", "));
  return parts.length > 0 ? parts.join(" · ") : t("placement.none");
}

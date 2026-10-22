import type { GPU, PhysicalGPU } from "../api";

/**
 * How one accelerator is told apart from another in a list.
 *
 * Not by its device index: the vendor CLI hands those out only to the cards
 * its driver brought up, so a node can hold several cards that have none
 * (passed through to a virtual machine, held by no driver, or one the
 * driver could not bring up). Keyed by index, those cards would collide
 * with each other and React would reuse one row's state for another.
 *
 * The order matches the server's own key for a card's row
 * (store.cardIdent): the PCI address first, since it is the slot the card
 * sits in, then the UUID, then the index for a prober that reports neither.
 */
export function cardKey(gpu: Pick<GPU, "pcie_bdf" | "uuid" | "index">): string {
  if (gpu.pcie_bdf) return `pci:${normalizeBDF(gpu.pcie_bdf)}`;
  if (gpu.uuid) return `uuid:${gpu.uuid}`;
  return `index:${gpu.index}`;
}

/**
 * A PCI address as the server spells it (accelerator.NormalizeBDF): lower
 * case and cut to the trailing "domain:bus:device.function", so a tool
 * that prints eight domain digits and one that prints four name the same
 * card. Without the cut, the same card keyed two different ways depending
 * on which tool reported it.
 */
function normalizeBDF(bdf: string): string {
  const trimmed = bdf.trim().toLowerCase();
  return trimmed.length > 12 ? trimmed.slice(-12) : trimmed;
}

/**
 * A card's key across the whole fleet. PCI addresses repeat from node to
 * node (every host has a 3b:00.0), so the fleet-wide GPU list scopes the
 * card's own key by its node; this is also what a `?gpu=` deep link names
 * for a card that has no UUID.
 */
export function physicalCardKey(gpu: Pick<PhysicalGPU, "node_uuid" | "pcie_bdf" | "uuid" | "index">): string {
  return `${gpu.node_uuid}/${cardKey(gpu)}`;
}

/**
 * Whether the card has metrics and a detail page of its own. Only cards the
 * driver answered for do: they have a UUID and the device number the
 * metrics are filed under. The rest are known by their PCI address alone,
 * so linking to a page or a chart about them would lead nowhere.
 */
export function cardHasDetail(gpu: Pick<GPU, "uuid" | "index">): boolean {
  return !!gpu.uuid && gpu.index >= 0;
}

/**
 * What names the card to a person. The device index when the vendor CLI
 * gave it one, otherwise its PCI address, since "GPU -1" names nothing and
 * the same words would then head several different cards.
 */
export function cardOrdinal(gpu: Pick<GPU, "index" | "pcie_bdf">): string {
  if (gpu.index >= 0) return String(gpu.index);
  return gpu.pcie_bdf || "—";
}

/**
 * Whether a person may pick the card by its device number for a deployment.
 * Both halves matter: a card with no number cannot be named to the engine
 * at all, and one the server says takes no work would be refused anyway.
 */
export function cardPickable(gpu: Pick<GPU, "index" | "schedulable">): boolean {
  return gpu.index >= 0 && gpu.schedulable;
}

/**
 * The cards of a node by model, the way a person reads a mixed box:
 * "1 × Quadro RTX 8000 · 1 × GeForce RTX 2080 Ti". Naming them all after the
 * first card ("2 × Quadro RTX 8000") hid the very card a slice was going to.
 */
export function gpuModelSummary(node: { gpu_count?: number; gpus?: Array<Pick<GPU, "name">> | null }): string {
  const counts = new Map<string, number>();
  for (const gpu of node.gpus ?? []) {
    const name = (gpu.name || "GPU").replace(/^NVIDIA\s+/i, "");
    counts.set(name, (counts.get(name) ?? 0) + 1);
  }
  if (!counts.size) return `${node.gpu_count ?? 0} × GPU`;
  return [...counts].map(([name, count]) => `${count} × ${name}`).join(" · ");
}

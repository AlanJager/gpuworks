// The GPU tab's figures and table rows, derived from the API's per-card rows.
import type { GpuGroupBy, GPUCardRow, GPUFigures } from "../../analyticsApi";

/** A card whose utilization averages below this over the range is idle. */
export const IDLE_UTIL = 5;

export function mean(values: (number | null)[]): number | null {
  const present = values.filter((v): v is number => v != null);
  return present.length ? present.reduce((a, b) => a + b, 0) / present.length : null;
}

export function maxOf(values: (number | null)[]): number | null {
  const present = values.filter((v): v is number => v != null);
  return present.length ? Math.max(...present) : null;
}

/** memPercent is a card's mean memory use over its own memory; null without both. */
export function memPercent(used: number | null, total: number | null): number | null {
  return used != null && total != null && total > 0 ? (used / total) * 100 : null;
}

export function isIdle(util: number | null): boolean {
  return util != null && util < IDLE_UTIL;
}

export interface GpuSummary {
  cards: number;
  byModel: { model: string; count: number }[];
  utilAvg: number | null;
  utilMax: number | null;
  memPercentAvg: number | null;
  memUsedAvg: number | null;
  /** The cards' mean power added up: what the whole set draws. */
  powerTotal: number | null;
  powerCards: number;
  idle: number;
  /** Cards with a utilization figure: the idle count's denominator. */
  measured: number;
}

export function summarize(cards: GPUCardRow[], totals: GPUFigures): GpuSummary {
  const models = new Map<string, number>();
  for (const c of cards) models.set(c.gpu_model, (models.get(c.gpu_model) ?? 0) + 1);
  const powers = cards.map((c) => c.power_avg_watts).filter((p): p is number => p != null);
  const measured = cards.filter((c) => c.util_avg != null);
  return {
    cards: cards.length,
    byModel: [...models].map(([model, count]) => ({ model, count })),
    utilAvg: totals.util_avg,
    utilMax: totals.util_max,
    memPercentAvg: mean(cards.map((c) => memPercent(c.mem_used_avg_bytes, c.mem_total_bytes))),
    memUsedAvg: mean(cards.map((c) => c.mem_used_avg_bytes)),
    powerTotal: powers.length ? powers.reduce((a, b) => a + b, 0) : null,
    powerCards: powers.length,
    idle: measured.filter((c) => isIdle(c.util_avg)).length,
    measured: measured.length,
  };
}

export interface GpuRow {
  id: string;
  /** What the row is: a card, a node or a card model. */
  title: string;
  sub: string;
  cards: GPUCardRow[];
  /** Cards in the row no longer in the inventory. */
  removed: number;
  utilAvg: number | null;
  utilMax: number | null;
  memAvg: number | null;
  memAvgPercent: number | null;
  memMax: number | null;
  memMaxPercent: number | null;
  powerAvg: number | null;
  tempMax: number | null;
  /** The deployments on the cards, each with the number of cards it holds. */
  deployments: { name: string; cards: number }[];
}

const cardId = (c: GPUCardRow) => c.gpu_uuid || `${c.node_uuid}:${c.gpu_index}`;

function rowOf(id: string, title: string, sub: string, cards: GPUCardRow[]): GpuRow {
  const held = new Map<string, number>();
  for (const c of cards) for (const d of c.deployments ?? []) held.set(d, (held.get(d) ?? 0) + 1);
  return {
    id,
    title,
    sub,
    cards,
    removed: cards.filter((c) => !c.present).length,
    utilAvg: mean(cards.map((c) => c.util_avg)),
    utilMax: maxOf(cards.map((c) => c.util_max)),
    memAvg: mean(cards.map((c) => c.mem_used_avg_bytes)),
    memAvgPercent: mean(cards.map((c) => memPercent(c.mem_used_avg_bytes, c.mem_total_bytes))),
    memMax: maxOf(cards.map((c) => c.mem_used_max_bytes)),
    memMaxPercent: maxOf(cards.map((c) => memPercent(c.mem_used_max_bytes, c.mem_total_bytes))),
    powerAvg: mean(cards.map((c) => c.power_avg_watts)),
    tempMax: maxOf(cards.map((c) => c.temp_max_celsius)),
    deployments: [...held].map(([name, count]) => ({ name, cards: count })),
  };
}

/** groupCards is the table's rows: one per card, or the cards grouped by node or card model. */
export function groupCards(cards: GPUCardRow[], by: GpuGroupBy): GpuRow[] {
  if (by === "gpu") {
    return cards.map((c) => rowOf(cardId(c), `${c.node_name || c.node_uuid} #${c.gpu_index}`, c.gpu_model, [c]));
  }
  const groups = new Map<string, GPUCardRow[]>();
  for (const c of cards) {
    const key = by === "node" ? c.node_uuid : c.gpu_model;
    groups.set(key, [...(groups.get(key) ?? []), c]);
  }
  return [...groups].map(([key, list]) => {
    if (by === "node") return rowOf(key, list[0].node_name || key, list[0].gpu_model, list);
    return rowOf(key, key || "?", [...new Set(list.map((c) => c.node_name || c.node_uuid))].join(" / "), list);
  });
}

export interface FilterOption {
  value: string;
  label: string;
}

/** filterOptions are the nodes and card models in cards, once each, for the node and model filters. */
export function filterOptions(cards: GPUCardRow[]): { nodes: FilterOption[]; models: FilterOption[] } {
  const nodes = new Map<string, string>();
  const models = new Set<string>();
  for (const c of cards) {
    nodes.set(c.node_uuid, c.node_name || c.node_uuid);
    if (c.gpu_model) models.add(c.gpu_model);
  }
  return {
    nodes: [...nodes].map(([value, label]) => ({ value, label })).sort((a, b) => a.label.localeCompare(b.label)),
    models: [...models].sort().map((m) => ({ value: m, label: m })),
  };
}

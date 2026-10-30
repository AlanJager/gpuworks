import type { OverviewDeployment, SchedulingOverview } from "../../api";

// The scheduling overview as the heatmap works with it: the response's compact
// arrays decoded into cards, and the filtering, grouping and sorting of the
// whole fleet, all in the browser from the one response. Nothing here touches
// the DOM, so it is tested on its own (overviewModel.test.ts).

/**
 * What a card is. The server derives it (the overview's `states`, which each
 * card refers to by position); the page only names and colours it.
 */
export type CellState = "offline" | "unhealthy" | "held_outside" | "awake" | "asleep" | "free";

/** The order the legend and the summary show the states in. */
export const CELL_STATES: CellState[] = ["awake", "asleep", "free", "held_outside", "unhealthy", "offline"];

export interface Occupant {
  /** Position in OverviewModel.deployments. */
  deployment: number;
  asleep: boolean;
}

export interface CardView {
  node: NodeView;
  index: number;
  model: string;
  memoryMiB: number;
  awakePct: number;
  sleepingPct: number;
  switching: boolean;
  occupants: Occupant[];
  state: CellState;
}

export interface NodeView {
  uuid: string;
  name: string;
  online: boolean;
  labels: Record<string, string>;
  contention: number;
  cards: CardView[];
}

export interface OverviewModel {
  nodes: NodeView[];
  deployments: OverviewDeployment[];
  models: string[];
  /** Every label key=value among the nodes, by key. */
  labels: Array<{ key: string; values: string[] }>;
}

function nodeViewOf(node: SchedulingOverview["nodes"][number], models: string[], states: string[]): NodeView {
  const view: NodeView = { uuid: node.uuid, name: node.name, online: node.online, labels: node.labels ?? {}, contention: node.contention ?? 0, cards: [] };
  for (const [index, model, memoryMiB, awakePct, sleepingPct, state, switching, occupants] of node.cards) {
    view.cards.push({
      node: view, index, model: models[model] ?? "", memoryMiB, awakePct, sleepingPct,
      switching: switching === 1,
      occupants: (occupants ?? []).map((code) => ({ deployment: code >> 1, asleep: (code & 1) === 1 })),
      state: states[state] as CellState,
    });
  }
  return view;
}

/**
 * Decodes the response. A node whose cards, labels and contention are the same
 * as in `previous` keeps its previous view, so the polling that finds a
 * quiet fleet hands the grid the same objects and it renders nothing again.
 */
export function buildModel(overview: SchedulingOverview, previous?: Map<string, { signature: string; view: NodeView }>): { model: OverviewModel; seen: Map<string, { signature: string; view: NodeView }> } {
  const seen = new Map<string, { signature: string; view: NodeView }>();
  const nodes = overview.nodes.map((node) => {
    const signature = JSON.stringify([node.name, node.online, node.contention, node.labels, node.cards, overview.models, overview.states]);
    const known = previous?.get(node.uuid);
    const entry = known && known.signature === signature ? known : { signature, view: nodeViewOf(node, overview.models, overview.states) };
    seen.set(node.uuid, entry);
    return entry.view;
  });
  const labels = new Map<string, Set<string>>();
  for (const node of nodes) {
    for (const [key, value] of Object.entries(node.labels)) {
      if (!labels.has(key)) labels.set(key, new Set());
      labels.get(key)!.add(value);
    }
  }
  return {
    model: {
      nodes, deployments: overview.deployments, models: overview.models,
      labels: [...labels.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([key, values]) => ({ key, values: [...values].sort(compareNames) })),
    },
    seen,
  };
}

export type StateFilter = "" | "free" | "sleepers" | "held" | "offline";
export type GroupBy = "node" | "model" | `label:${string}`;
export type SortBy = "name" | "free" | "contention";

export interface Filters {
  /** GPU models to keep; none keeps all. */
  models: string[];
  /** "key=value" of a node label; empty keeps all. */
  label: string;
  /** Keep the nodes that have free cards, sleepers, held cards or are offline. */
  state: StateFilter;
  /** A deployment name (a part of it) whose cards are highlighted. */
  search: string;
}

export const NO_FILTERS: Filters = { models: [], label: "", state: "", search: "" };

export interface GridNode {
  node: NodeView;
  cards: CardView[];
}

export interface Section {
  key: string;
  /** The model or label value the section groups by; empty when not grouped. */
  label: string;
  nodes: GridNode[];
  cards: number;
  free: number;
}

export interface Arrangement {
  sections: Section[];
  nodes: number;
  cards: number;
  /** Positions of the deployments the search names. */
  matched: Set<number>;
  /** Cards occupied by a matched deployment, among those shown. */
  highlighted: number;
}

/** compareNames orders names the way people count: gpu-2 before gpu-10. */
export function compareNames(a: string, b: string): number {
  return a.localeCompare(b, undefined, { numeric: true, sensitivity: "base" });
}

/** freeMiB is the memory a node can still give: its online, usable cards' unclaimed share. */
export function freeMiB(cards: CardView[]): number {
  let total = 0;
  for (const card of cards) {
    if (card.state === "offline" || card.state === "unhealthy" || card.state === "held_outside") continue;
    total += (card.memoryMiB * Math.max(100 - card.awakePct - card.sleepingPct, 0)) / 100;
  }
  return total;
}

function nodeMatches(node: NodeView, cards: CardView[], filters: Filters): boolean {
  if (filters.label) {
    const at = filters.label.indexOf("=");
    const key = filters.label.slice(0, at);
    if (at < 0 || node.labels[key] !== filters.label.slice(at + 1)) return false;
  }
  switch (filters.state) {
    case "free": return cards.some((card) => card.state === "free");
    case "sleepers": return cards.some((card) => card.occupants.some((o) => o.asleep));
    case "held": return cards.some((card) => card.state === "held_outside");
    case "offline": return !node.online;
    default: return true;
  }
}

const NO_LABEL = "";

function sectionOf(groupBy: GroupBy, node: NodeView, card: CardView): string {
  if (groupBy === "model") return card.model;
  if (groupBy.startsWith("label:")) return node.labels[groupBy.slice("label:".length)] ?? NO_LABEL;
  return "";
}

function sorter(sortBy: SortBy): (a: GridNode, b: GridNode) => number {
  switch (sortBy) {
    case "free": return (a, b) => freeMiB(b.cards) - freeMiB(a.cards) || compareNames(a.node.name, b.node.name);
    case "contention": return (a, b) => b.node.contention - a.node.contention || compareNames(a.node.name, b.node.name);
    default: return (a, b) => compareNames(a.node.name, b.node.name);
  }
}

/**
 * Applies the filters, groups what is left and sorts the nodes. A node with
 * mixed models appears in each model's section with only its cards of that
 * model; a node with no card left after the model filter is dropped.
 */
export function arrange(model: OverviewModel, filters: Filters, groupBy: GroupBy, sortBy: SortBy): Arrangement {
  const wanted = new Set(filters.models);
  const needle = filters.search.trim().toLowerCase();
  const matched = new Set<number>();
  if (needle) model.deployments.forEach((dep, at) => { if (dep.name.toLowerCase().includes(needle)) matched.add(at); });
  const sections = new Map<string, Section>();
  let nodes = 0;
  let cards = 0;
  let highlighted = 0;
  const shownNodes = new Set<string>();
  for (const node of model.nodes) {
    const kept = wanted.size ? node.cards.filter((card) => wanted.has(card.model)) : node.cards;
    if (kept.length === 0 || !nodeMatches(node, kept, filters)) continue;
    const bySection = new Map<string, CardView[]>();
    for (const card of kept) {
      const key = sectionOf(groupBy, node, card);
      const list = bySection.get(key);
      if (list) list.push(card);
      else bySection.set(key, [card]);
    }
    for (const [key, sectionCards] of bySection) {
      let section = sections.get(key);
      if (!section) {
        section = { key: key || (groupBy === "node" ? "all" : "none"), label: key, nodes: [], cards: 0, free: 0 };
        sections.set(key, section);
      }
      section.nodes.push({ node, cards: sectionCards });
      section.cards += sectionCards.length;
      for (const card of sectionCards) {
        if (card.state === "free") section.free += 1;
        if (matched.size && card.occupants.some((o) => matched.has(o.deployment))) highlighted += 1;
      }
      cards += sectionCards.length;
      shownNodes.add(node.uuid);
    }
  }
  nodes = shownNodes.size;
  const order = sorter(sortBy);
  const list = [...sections.values()];
  for (const section of list) section.nodes.sort(order);
  // Sections: models by their size, label values by name, the unlabelled last.
  if (groupBy === "model") list.sort((a, b) => b.cards - a.cards || compareNames(a.label, b.label));
  else if (groupBy !== "node") list.sort((a, b) => (a.label === NO_LABEL ? 1 : 0) - (b.label === NO_LABEL ? 1 : 0) || compareNames(a.label, b.label));
  return { sections: list, nodes, cards, matched, highlighted };
}

/** How many cards of the arrangement are in each state. */
export function countStates(arrangement: Arrangement): Record<CellState, number> {
  const counts: Record<CellState, number> = { awake: 0, asleep: 0, free: 0, held_outside: 0, unhealthy: 0, offline: 0 };
  for (const section of arrangement.sections) for (const item of section.nodes) for (const card of item.cards) counts[card.state] += 1;
  return counts;
}

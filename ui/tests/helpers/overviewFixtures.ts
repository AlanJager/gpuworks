import type { OverviewCardWire, OverviewNode, SchedulingOverview } from "../../src/schedulingApi.ts";

/** The states the server names, in its order: a card refers to one by position. */
export const STATES = ["offline", "unhealthy", "held_outside", "awake", "asleep", "free"];

export interface CardOptions {
  /** The state the server derived; awake, asleep or free by the shares when not given. */
  state?: string;
  switching?: boolean;
}

// A compact card as the server writes it: [index, model, memory_mib, awake_pct,
// sleeping_pct, state, switching, occupants?]. A card of an offline node is
// rewritten offline by overviewOf, as the server writes it.
export const card = (index: number, model: number, awake = 0, sleeping = 0, options: CardOptions = {}, occupants?: number[]): OverviewCardWire => {
  const state = STATES.indexOf(options.state ?? (awake > 0 ? "awake" : sleeping > 0 ? "asleep" : "free"));
  if (state < 0) throw new Error(`no state ${options.state}`);
  const switching = options.switching ? 1 : 0;
  return occupants ? [index, model, 49152, awake, sleeping, state, switching, occupants] : [index, model, 49152, awake, sleeping, state, switching];
};

const ZERO = { cards: 0, awake: 0, asleep: 0, free: 0, held_outside: 0, unhealthy: 0, offline: 0 };

/** overviewOf wraps nodes in a response whose summary is counted from the cards' states, as the server counts. */
export function overviewOf(nodes: OverviewNode[], models: string[], deployments: SchedulingOverview["deployments"] = [], contention = { waiting_wakes: 0, held_requests: 0, yields_last_hour: 0 }): SchedulingOverview {
  const summary = { ...ZERO, nodes: nodes.length, online_nodes: nodes.filter((n) => n.online).length };
  for (const node of nodes) {
    if (!node.online) node.cards = node.cards.map((wire) => { const out = [...wire] as OverviewCardWire; out[5] = 0; return out; });
    for (const [, , , , , state] of node.cards) {
      const bucket = STATES[state] as keyof typeof ZERO;
      summary.cards += 1;
      summary[bucket] += 1;
    }
  }
  return { summary: { ...summary, contention }, states: STATES, models, deployments, nodes };
}

/** The small fleet most tests read: three nodes, two GPU models, three deployments. */
export function smallFleet(): SchedulingOverview {
  return overviewOf(
    [
      { uuid: "node-a", name: "gpu-a", online: true, labels: { zone: "a" }, contention: 5, cards: [card(0, 0, 100, 0, {}, [0]), card(1, 0, 25, 4, { switching: true }, [2, 5])] },
      { uuid: "node-b", name: "gpu-b", online: false, cards: [card(0, 0, 100, 0, {}, [0])] },
      { uuid: "node-c", name: "gpu-c", online: true, labels: { zone: "c" }, cards: [card(0, 1, 0, 0, { state: "held_outside" }), card(1, 1, 0, 5, {}, [5]), card(2, 1)] },
    ],
    ["L20", "H100"],
    [{ id: 7, name: "qwen", yield_policy: "never" }, { id: 8, name: "batch", yield_policy: "timeslice" }, { id: 9, name: "night", yield_policy: "idle" }],
    { waiting_wakes: 1, held_requests: 3, yields_last_hour: 2 },
  );
}

/** A fleet of `nodes` nodes of `cards` cards each, in every state, for the scale cases. */
export function bigFleet(nodes: number, cards: number): SchedulingOverview {
  const list: OverviewNode[] = [];
  for (let i = 0; i < nodes; i += 1) {
    list.push({
      uuid: `node-${i}`, name: `gpu-${String(i).padStart(4, "0")}`, online: i % 25 !== 0, labels: { zone: `z${i % 4}` }, contention: i % 7,
      cards: Array.from({ length: cards }, (_, c) => {
        const pick = (i + c) % 6;
        const options: CardOptions = pick === 2 ? { state: "held_outside" } : pick === 3 ? { switching: true } : {};
        return card(c, i % 3 === 0 ? 1 : 0, pick === 0 ? 40 + c * 7 : 0, pick === 1 ? 5 : 0, options, pick <= 1 ? [(i % 3) * 2 + (pick === 1 ? 1 : 0)] : undefined);
      }),
    });
  }
  return overviewOf(list, ["L20", "H100"], [{ id: 1, name: "alpha", yield_policy: "never" }, { id: 2, name: "beta", yield_policy: "timeslice" }, { id: 3, name: "gamma", yield_policy: "anytime" }]);
}

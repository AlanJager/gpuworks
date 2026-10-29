import type { OverviewCardWire, OverviewNode, SchedulingOverview } from "../../src/api.ts";

// A compact card as the server writes it: [index, model, memory_mib, awake_pct,
// sleeping_pct, flags, occupants?].
export const card = (index: number, model: number, awake = 0, sleeping = 0, flags = 0, occupants?: number[]): OverviewCardWire =>
  occupants ? [index, model, 49152, awake, sleeping, flags, occupants] : [index, model, 49152, awake, sleeping, flags];

export const FLAGS = { held: 1, switching: 2, unhealthy: 4 };

const ZERO = { cards: 0, awake: 0, asleep: 0, free: 0, held_outside: 0, unhealthy: 0, offline: 0 };

/** overviewOf wraps nodes in a response whose summary is counted from them, as the server counts. */
export function overviewOf(nodes: OverviewNode[], models: string[], deployments: SchedulingOverview["deployments"] = [], contention = { waiting_wakes: 0, held_requests: 0, yields_last_hour: 0 }): SchedulingOverview {
  const summary = { ...ZERO, nodes: nodes.length, online_nodes: nodes.filter((n) => n.online).length };
  const byModel = new Map<string, typeof ZERO & { model: string }>();
  for (const node of nodes) {
    for (const [, model, , awake, sleeping, flags] of node.cards) {
      const row = byModel.get(models[model]) ?? { model: models[model], ...ZERO };
      byModel.set(models[model], row);
      const bucket = !node.online ? "offline" : flags & FLAGS.unhealthy ? "unhealthy" : flags & FLAGS.held ? "held_outside" : awake > 0 ? "awake" : sleeping > 0 ? "asleep" : "free";
      for (const target of [summary, row]) {
        target.cards += 1;
        target[bucket] += 1;
      }
    }
  }
  return { summary: { ...summary, by_model: [...byModel.values()], contention }, models, deployments, nodes };
}

/** The small fleet most tests read: three nodes, two GPU models, three deployments. */
export function smallFleet(): SchedulingOverview {
  return overviewOf(
    [
      { uuid: "node-a", name: "gpu-a", online: true, labels: { zone: "a" }, contention: 5, cards: [card(0, 0, 100, 0, 0, [0]), card(1, 0, 25, 4, FLAGS.switching, [2, 5])] },
      { uuid: "node-b", name: "gpu-b", online: false, cards: [card(0, 0, 100, 0, 0, [0])] },
      { uuid: "node-c", name: "gpu-c", online: true, labels: { zone: "c" }, cards: [card(0, 1, 0, 0, FLAGS.held), card(1, 1, 0, 5, 0, [5]), card(2, 1)] },
    ],
    ["L20", "H100"],
    [{ id: 7, name: "qwen", preset: "resident" }, { id: 8, name: "batch", preset: "shared" }, { id: 9, name: "night", preset: "online" }],
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
        return card(c, i % 3 === 0 ? 1 : 0, pick === 0 ? 40 + c * 7 : 0, pick === 1 ? 5 : 0, pick === 2 ? FLAGS.held : pick === 3 ? FLAGS.switching : 0, pick <= 1 ? [(i % 3) * 2 + (pick === 1 ? 1 : 0)] : undefined);
      }),
    });
  }
  return overviewOf(list, ["L20", "H100"], [{ id: 1, name: "alpha", preset: "resident" }, { id: 2, name: "beta", preset: "shared" }, { id: 3, name: "gamma", preset: "offline" }]);
}

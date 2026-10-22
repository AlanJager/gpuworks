import type { Instance } from "../api";

// A rank of a multi-node replica as the instance list shows it: rank 0 leads
// and serves, the rest are headless followers; a follower names its
// generation's leader so several multi-node replicas of one role can be told
// apart.
export interface DistRank {
  role: "leader" | "follower";
  rank: number;
  leaderUUID?: string;
}

// isMultiNodeRank reports whether the instance is one rank of a multi-node
// replica. A single-rank replica carries dist_role "leader" and nnodes 1 too,
// so dist_role alone does not say it.
export function isMultiNodeRank(inst: Instance): boolean {
  return (inst.nnodes ?? 0) > 1 || !!inst.headless || (inst.node_rank ?? 0) > 0;
}

// isFollower reports whether the instance follows in a multi-node
// generation; the server derives dist_role for rows from before it was
// recorded.
export function isFollower(inst: Instance): boolean {
  return inst.dist_role === "follower";
}

function generationKey(inst: Instance): string | undefined {
  if (!isMultiNodeRank(inst) || !inst.dist_group) return undefined;
  return `${inst.dist_group}#${inst.dist_attempt ?? 0}`;
}

function rankOrder(a: Instance, b: Instance): number {
  const byRole = Number(isFollower(a)) - Number(isFollower(b));
  return byRole || (a.node_rank ?? 0) - (b.node_rank ?? 0);
}

// orderByGeneration keeps the list order but moves each generation's ranks
// next to each other, leader first and followers by rank, where the first of
// them appeared. Single-rank replicas and rows without a generation stay put.
export function orderByGeneration(instances: Instance[]): Instance[] {
  const groups = new Map<string, Instance[]>();
  const slots: Array<Instance | string> = [];
  for (const inst of instances) {
    const key = generationKey(inst);
    if (!key) {
      slots.push(inst);
      continue;
    }
    const group = groups.get(key);
    if (group) group.push(inst);
    else {
      groups.set(key, [inst]);
      slots.push(key);
    }
  }
  return slots.flatMap((slot) => (typeof slot === "string" ? [...groups.get(slot)!].sort(rankOrder) : [slot]));
}

// distRank describes a multi-node rank, undefined for a single-rank replica.
export function distRank(inst: Instance, instances: Instance[]): DistRank | undefined {
  if (!isMultiNodeRank(inst)) return undefined;
  const rank = inst.node_rank ?? 0;
  if (!isFollower(inst)) return { role: "leader", rank };
  const key = generationKey(inst);
  const leader = key ? instances.find((other) => generationKey(other) === key && !isFollower(other)) : undefined;
  return { role: "follower", rank, leaderUUID: leader?.uuid };
}

// followerCount is how many listed instances are followers: the drawer counts
// them apart so a two-node replica does not read as two replicas.
export function followerCount(instances: Instance[]): number {
  return instances.filter((inst) => isMultiNodeRank(inst) && isFollower(inst)).length;
}

// instanceSleepLevel is the level an instance sleeps at or is ordered to:
// an order counts at once, as the gateway stops sending it requests then.
export function instanceSleepLevel(inst: Instance): number {
  return Math.max(inst.sleep_level ?? 0, inst.desired_sleep ?? 0);
}

// SleepSummary counts the replicas (their leaders) that sleep or are ordered
// to, and the deepest level among them.
export interface SleepSummary {
  asleep: number;
  total: number;
  level: number;
}

export function sleepSummary(instances: Instance[], role?: string): SleepSummary {
  const leaders = instances.filter((inst) => inst.replica && !isFollower(inst) && (role === undefined || inst.replica.role === role));
  return {
    asleep: leaders.filter((inst) => inst.asleep).length,
    total: leaders.length,
    level: Math.max(0, ...leaders.map(instanceSleepLevel)),
  };
}

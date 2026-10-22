import type { Instance } from "../api";
import { isFollower, isMultiNodeRank } from "./instanceGroups";

// The image ID an instance reports (`image_digest_id`, "sha256:…") is the
// identity of the image its container actually runs. Replicas of one
// deployment normally run the same one; they differ when a tag was updated
// between two pulls, or when different sources served different content. The
// platform pulls by tag and does not check the ID, so the drawer shows it and
// flags the odd ones out.

/** Where the comparison happens, and what it found. */
export interface ImageMismatch {
  /** The image ID the replicas of each group are measured against, keyed by PD role ("" outside PD). */
  references: Map<string, string>;
  /** The uuids of the replicas whose ID differs from their group's reference. */
  mismatched: Set<string>;
}

// An ended instance keeps the image of its day and says nothing about what the
// deployment runs now.
const ended = new Set(["stopped", "failed"]);

const idOf = (inst: Instance) => (inst.image_digest_id ?? "").trim();

/**
 * imageMismatch finds the replicas that run a different image from the rest.
 *
 * What is compared: replica-level instances only. The ranks of one multi-node
 * replica are launched as one unit from one image, so the followers are left
 * out and the leader stands for the replica. A PD deployment compares inside
 * each role, because the prefill and decode roles are separate engines that
 * may legitimately use different images. Ended instances are ignored.
 *
 * The reference is the most frequent non-empty ID of the group. With no unique
 * most frequent ID (1 against 1, 2 against 2) the group of the oldest replica
 * is the reference and the others are flagged: the older replicas are what the
 * tag meant when the deployment first ran. The instance DTO carries no
 * creation time, so "oldest" is the last one listed, because the API lists a
 * deployment's instances newest first.
 *
 * A replica without an ID is never flagged and does not count as a vote; a
 * group with fewer than two IDs cannot disagree.
 */
export function imageMismatch(instances: Instance[]): ImageMismatch {
  const groups = new Map<string, Instance[]>();
  for (const inst of instances) {
    if (ended.has(inst.state)) continue;
    if (isMultiNodeRank(inst) && isFollower(inst)) continue;
    const key = inst.role ?? "";
    groups.set(key, [...(groups.get(key) ?? []), inst]);
  }
  const references = new Map<string, string>();
  const mismatched = new Set<string>();
  for (const [key, members] of groups) {
    const withID = members.filter((inst) => idOf(inst) !== "");
    const votes = new Map<string, number>();
    for (const inst of withID) votes.set(idOf(inst), (votes.get(idOf(inst)) ?? 0) + 1);
    if (votes.size === 0) continue;
    const top = Math.max(...votes.values());
    const leaders = new Set([...votes].filter(([, count]) => count === top).map(([id]) => id));
    const reference = leaders.size === 1
      ? [...leaders][0]
      : idOf([...withID].reverse().find((inst) => leaders.has(idOf(inst)))!);
    references.set(key, reference);
    for (const inst of withID) if (idOf(inst) !== reference) mismatched.add(inst.uuid);
  }
  return { references, mismatched };
}

/** shortImageID is "sha256:" and the first 8 hex characters, with an ellipsis when it cut something off. */
export function shortImageID(id?: string): string {
  const value = (id ?? "").trim();
  const match = /^(sha256:)?(.*)$/i.exec(value);
  const prefix = match?.[1] ? "sha256:" : "";
  const hex = match?.[2] ?? value;
  return `${prefix}${hex.slice(0, 8)}${hex.length > 8 ? "…" : ""}`;
}

import type { CommunityTag } from "../../api";

/**
 * searchCommunityTags narrows an upstream tag list by name. The server lists
 * every tag a repository has — release, nightly, model-specific tags such as
 * `qwen38` — so the only filter left is the one the operator types.
 */
export function searchCommunityTags(tags: CommunityTag[], query: string): CommunityTag[] {
  const needle = query.trim().toLowerCase();
  if (!needle) return tags;
  return tags.filter((item) => item.tag.toLowerCase().includes(needle));
}

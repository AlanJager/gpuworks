import type { Model } from "../../api";

const commit = /^[0-9a-f]{40}$/i;

// A Hugging Face / ModelScope model whose revision is empty or a branch name
// names no fixed commit. With a copy registered from a node path, other nodes
// that later download it from the site may get different files than that copy.
// Only the detail DTO lists where the files are, so the list rows cannot tell.
export function hasUnpinnedLocalCopy(model: Pick<Model, "source" | "revision" | "nodes">): boolean {
  const source = model.source.trim().toLowerCase();
  if (source !== "huggingface" && source !== "modelscope") return false;
  if (commit.test((model.revision ?? "").trim())) return false;
  return (model.nodes ?? []).some((item) => item.acquisition === "registered" && !item.cache);
}

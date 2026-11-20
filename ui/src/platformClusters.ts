import type { ProviderCategoryInfo } from "./api";

/** The kinds of cluster the external platforms add as an entry with a cluster form, not as a vendor's account. */
export type ClusterFormKind = "kubernetes" | "slurm";

/** Which cluster form a platform type is added with, as the server flags it; null for a type that is a vendor's account. */
export function clusterFormKind(category: ProviderCategoryInfo): ClusterFormKind | null {
  if (category.kube_cluster) return "kubernetes";
  if (category.cluster_kind === "slurm") return "slurm";
  return null;
}

/** The platform type the server offers for one kind of cluster entry. */
export function clusterCategoryOf(categories: ProviderCategoryInfo[], kind: ClusterFormKind): ProviderCategoryInfo | undefined {
  return categories.find((category) => clusterFormKind(category) === kind);
}

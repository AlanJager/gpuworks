import type { CloudProvider, KindResource, KubeCluster } from "../../api";
import type { SlurmCluster } from "../../slurmApi";

/**
 * A row of the external platforms list: an account of a cloud vendor, or a
 * cluster connected on its own (a Kubernetes cluster with kubectl, a Slurm
 * cluster with a health check; not from an account). A cluster connected from
 * an account is not a row: it is listed under its account.
 */
export type PlatformRow =
  | { key: string; type: "provider"; provider: CloudProvider }
  | { key: string; type: "cluster"; cluster: KubeCluster }
  | { key: string; type: "slurm"; slurm: SlurmCluster };

export const providerRowKey = (id: number) => `p${id}`;
export const clusterRowKey = (uuid: string) => `k${uuid}`;
export const slurmRowKey = (uuid: string) => `s${uuid}`;

/** Accounts in the server's order, then the Kubernetes clusters connected on their own, then the Slurm ones. */
export function platformRows(providers: CloudProvider[], clusters: KubeCluster[], slurm: SlurmCluster[] = []): PlatformRow[] {
  return [
    ...providers.map((provider): PlatformRow => ({ key: providerRowKey(provider.id), type: "provider", provider })),
    ...clusters.filter((cluster) => !cluster.source).map((cluster): PlatformRow => ({ key: clusterRowKey(cluster.uuid), type: "cluster", cluster })),
    ...slurm.filter((entry) => !entry.source).map((entry): PlatformRow => ({ key: slurmRowKey(entry.uuid), type: "slurm", slurm: entry })),
  ];
}

export const rowName = (row: PlatformRow) => row.type === "provider" ? row.provider.name : row.type === "cluster" ? row.cluster.name : row.slurm.name;
/** The vendor of an account; a cluster connected on its own is no vendor's. */
export const rowKind = (row: PlatformRow) => row.type === "provider" ? row.provider.kind : "";
/** The platform type, as the server files the account or the cluster entry. */
export const rowCategory = (row: PlatformRow): string => row.type === "provider" ? row.provider.category : row.type === "cluster" ? row.cluster.category : row.slurm.category;
export const rowNodeCount = (row: PlatformRow) => row.type === "provider" ? row.provider.node_count ?? 0 : row.type === "cluster" ? row.cluster.node_count : row.slurm.node_count;
export const rowUpdatedAt = (row: PlatformRow) => row.type === "provider" ? row.provider.updated_at : row.type === "cluster" ? row.cluster.created_at : row.slurm.created_at;

/** What the row connects, as the server says. */
export function rowResources(row: PlatformRow): KindResource[] {
  return (row.type === "provider" ? row.provider.resources : row.type === "cluster" ? row.cluster.resources : row.slurm.resources) ?? [];
}

/**
 * The row's state as the status filter names it: an account's own state
 * (valid, invalid), or where the server says a cluster's entry stands.
 */
export const rowState = (row: PlatformRow): string => row.type === "provider" ? row.provider.state : row.type === "cluster" ? row.cluster.state : row.slurm.state;

/** The entry's UUID of a row that is a cluster connected on its own; the node list filters by it. */
export const rowClusterUUID = (row: Exclude<PlatformRow, { type: "provider" }>): string => row.type === "cluster" ? row.cluster.uuid : row.slurm.uuid;

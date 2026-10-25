# ADR 0062: After User Confirmation, the Server Applies the Connect Manifest to a Managed Cluster with a Short-Lived kubeconfig Issued on the Spot by the Cloud Provider

Date: 2026-10-22. Status: Accepted (amends item 2 of [ADR 0052](0052-kubernetes-nodes.md), “the server holds no kubeconfig and does not connect to the apiserver”).

## Background

When connecting managed Kubernetes (Alibaba Cloud ACK, DigitalOcean DOKS), users must find the GPU node labels and run `kubectl apply` themselves. Yet the cloud account is already in gpuworks, and the cluster list, node pools, and cluster credentials can all be obtained from the cloud API. ADR 0052 decided that the server does not touch the cluster so as not to keep a long-lived, highly privileged kubeconfig.

## Decisions

1. **Clusters are listed live from the cloud account and are not stored.** A provider driver can implement `ClusterProvider` (list clusters and GPU node pools, issue a kubeconfig). When the user picks a GPU node pool to connect, an ordinary cluster entry is created; the entry records only the additional source (cloud account, cluster ID), and the node selector is generated from the node pool.
2. **The server connects to the cluster only within the single operation the user confirms.** Both “Apply to the cluster” and “Remove from the cluster” first run a server-side dry run that lists each object that would be created, updated, or deleted, and execute only after the user confirms. Each operation fetches a kubeconfig from the cloud API on the spot (valid for 15 minutes for both ACK and DOKS) and discards it after use; it is **neither stored in the database nor written to logs**. Only the certificate or token carried in the kubeconfig is honored; a kubeconfig that needs to run a program (exec, auth-provider) is rejected.
3. **Only gpuworks's own manifest is applied.** The server server-side applies (`fieldManager=gpuworks`) the fixed set of object kinds rendered by `kubemanifest.Render`, and rejects any other kind. Removal deletes the DaemonSet first (the agents, placeholder Pods, and instance Pods are deleted with it), and cluster-scoped objects (PriorityClass) still used by other gpuworks namespaces in the same cluster are kept.
4. **The manual path is unchanged.** Every entry still has a `kubectl apply` command; a cluster whose API Server has no public address can be connected only this way. An entry created by hand has no source, and the server never connects to it.
5. **Nodes of a managed cluster do not get an agent installed individually.** The cloud host list marks the cluster each belongs to, and the install endpoint refuses (`PROVIDER_INSTANCE_IN_CLUSTER`), to avoid contending with the DaemonSet for the same machine.

## Trade-offs

- Not storing the kubeconfig costs one extra cloud API call per operation, and the cloud account must have permission to issue cluster credentials (ACK also requires the sub-account to have permissions in the cluster's RBAC).
- There is no “automatic sync” or background reconciliation: cluster changes (node pool scaling) are followed by the DaemonSet itself, so the server does not need a standing connection.
- client-go is not introduced: the manifest has only nine kinds of objects, a minimal REST client is enough, and it matches the agent side (item 4 of ADR 0052).

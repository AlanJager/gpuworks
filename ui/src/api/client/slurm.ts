import type { SlurmApplyCheck, SlurmApplyOutcome, SlurmCluster, SlurmClusterEdit, SlurmClusterRequest, SlurmClusterUpdate, SlurmDefaults, SlurmRemoveOutcome, SlurmRemoveStep } from "../../slurmApi";
import { getJSON, sendJSON, type List } from "../http";
import { getText } from "./transport";

export const slurmApi = {
  slurmClusters: () => getJSON<List<SlurmCluster>>("/api/v1/slurm/clusters"),
  slurmDefaults: () => getJSON<SlurmDefaults>("/api/v1/slurm/defaults"),
  createSlurmCluster: (body: SlurmClusterRequest) => sendJSON<SlurmCluster>("POST", "/api/v1/slurm/clusters", body),
  updateSlurmCluster: (uuid: string, body: SlurmClusterUpdate) => sendJSON<SlurmClusterEdit>("PUT", `/api/v1/slurm/clusters/${encodeURIComponent(uuid)}`, body),
  revokeSlurmCluster: (uuid: string) => sendJSON<SlurmCluster>("POST", `/api/v1/slurm/clusters/${encodeURIComponent(uuid)}/revoke`),
  /** withOfflineNodes deletes the entry together with its nodes (409 SLURM_CLUSTER_NODES_ONLINE / SLURM_CLUSTER_NODES_HAVE_INSTANCES when the server will not). */
  deleteSlurmCluster: (uuid: string, withOfflineNodes = false) =>
    sendJSON<void>("DELETE", `/api/v1/slurm/clusters/${encodeURIComponent(uuid)}${withOfflineNodes ? "?with_offline_nodes=true" : ""}`),
  /** Runs the setup on the cluster's controller through the cloud account (admin; entries with can_apply). */
  applySlurmCluster: (uuid: string) => sendJSON<SlurmApplyOutcome>("POST", `/api/v1/slurm/clusters/${encodeURIComponent(uuid)}/apply`, { dry_run: false }),
  /** The setup script the controller is served, as text, with the enrollment token left out (admin). */
  slurmClusterScript: (uuid: string) => getText(`/api/v1/slurm/clusters/${encodeURIComponent(uuid)}/script`),
  /** The check alone: what applying would do, with nothing changed on the cluster. */
  checkSlurmCluster: (uuid: string) => sendJSON<SlurmApplyCheck>("POST", `/api/v1/slurm/clusters/${encodeURIComponent(uuid)}/apply`, { dry_run: true }),
  /** One uninstall step on the cluster's controller, through the cloud account (admin; entries with can_apply). */
  removeSlurmCluster: (uuid: string, step: SlurmRemoveStep) =>
    sendJSON<SlurmRemoveOutcome>("POST", `/api/v1/slurm/clusters/${encodeURIComponent(uuid)}/remove`, { step }),
};

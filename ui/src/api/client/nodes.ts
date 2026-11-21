import type { NetworkRole, NodeNetworks } from "../../networkTypes";
import { getJSON, listQueryString, sendJSON, type List, type ListQuery } from "../http";
import type { CardCheckDetail, CardHealthChecks, CardHealthRecords, CardRef } from "../cardHealth";
import type { CardScheduling } from "../gpus";
import { nodeFilterString, type FSListResponse, type Node, type NodeFacets, type NodeFilter, type NodePage, type NodeReplicas, type NodeScheduling } from "../nodes";
import type { RecoveryOutcome, SSHAuthMode, SSHCredentialInfo, SSHInstallReport, SSHInstallRequest, SSHPrecheckReport, SSHPrecheckRequest } from "../ssh";
import type { NodeDoctorReport } from "../doctor";
import type { NodeRuntime } from "../network";
import type { NodePullProxy } from "../pullSources";

const cardQuery = (card?: CardRef) => (card?.pcie_bdf ? `pcie_bdf=${encodeURIComponent(card.pcie_bdf)}` : card?.uuid ? `uuid=${encodeURIComponent(card.uuid)}` : "");

export const nodesApi = {
  nodes: () => getJSON<List<Node>>("/api/v1/nodes"),
  nodesPage: (query: ListQuery) => getJSON<NodePage>(`/api/v1/nodes?${listQueryString(query)}`),
  nodeFacets: (filter: NodeFilter = {}) => getJSON<NodeFacets>(`/api/v1/nodes/facets?${nodeFilterString(filter)}`),
  createNode: (
    input?:
      | string
      | {
          name?: string;
          connect_mode?: "agent_dial" | "server_dial";
          server_dial_listen?: string;
          host_address?: string;
          server_url?: string;
        },
  ) => {
    const body = typeof input === "string" ? { name: input } : (input ?? {});
    return sendJSON<{
      node: Node;
      token: string;
      install_command: string;
      server_url?: string;
      server_dial_address?: string;
    }>("POST", "/api/v1/nodes", body);
  },
  node: (uuid: string) => getJSON<Node>(`/api/v1/nodes/${uuid}`),
  nodeDoctor: (uuid: string) =>
    sendJSON<NodeDoctorReport>("POST", `/api/v1/nodes/${uuid}/doctor`),
  updateNodeAccelerator: (uuid: string, accelerator: string) =>
    sendJSON<Node>("PATCH", `/api/v1/nodes/${uuid}/accelerator`, { accelerator }),
  updateNodeRuntime: (
    uuid: string,
    runtime: "auto" | "docker" | "nerdctl" | "udocker",
  ) => sendJSON<Node>("PATCH", `/api/v1/nodes/${uuid}/runtime`, { runtime }),
  nodeFSList: (uuid: string, path: string) =>
    getJSON<FSListResponse>(
      `/api/v1/nodes/${uuid}/fs?path=${encodeURIComponent(path)}`,
    ),
  setNodeScheduling: (uuid: string, scheduling: NodeScheduling) =>
    sendJSON<Node>("PUT", `/api/v1/nodes/${encodeURIComponent(uuid)}/scheduling`, { scheduling }),
  nodeHealthChecks: (nodeUUID: string, card?: CardRef) => {
    const q = cardQuery(card);
    return getJSON<CardHealthChecks>(`/api/v1/nodes/${encodeURIComponent(nodeUUID)}/health-checks${q ? `?${q}` : ""}`);
  },
  nodeHealthRecords: (nodeUUID: string, opts: { card?: CardRef; check?: string; page: number; pageSize?: number }) => {
    const parts = [cardQuery(opts.card), opts.check ? `check=${encodeURIComponent(opts.check)}` : "", `page=${opts.page}`, `page_size=${opts.pageSize ?? 50}`].filter(Boolean);
    return getJSON<CardHealthRecords>(`/api/v1/nodes/${encodeURIComponent(nodeUUID)}/health-records?${parts.join("&")}`);
  },
  cardCheckDetail: (nodeUUID: string, card: CardRef, check: string) =>
    getJSON<CardCheckDetail>(`/api/v1/nodes/${encodeURIComponent(nodeUUID)}/health-checks/detail?${cardQuery(card)}&check=${encodeURIComponent(check)}`),
  recheckCard: (nodeUUID: string, card: CardRef) =>
    sendJSON<CardCheckDetail>("POST", `/api/v1/nodes/${encodeURIComponent(nodeUUID)}/gpus/recheck`, card.pcie_bdf ? { pcie_bdf: card.pcie_bdf } : { uuid: card.uuid }),
  setCardScheduling: (nodeUUID: string, card: { pcie_bdf?: string; uuid?: string }, scheduling: CardScheduling) =>
    sendJSON<Node>("PUT", `/api/v1/nodes/${encodeURIComponent(nodeUUID)}/gpus/scheduling`, {
      ...(card.pcie_bdf ? { pcie_bdf: card.pcie_bdf } : { uuid: card.uuid }),
      scheduling,
    }),
  nodeReplicas: (uuid: string) =>
    getJSON<NodeReplicas>(`/api/v1/nodes/${encodeURIComponent(uuid)}/replicas`),
  // evictNode takes every replica off a cordoned or draining node at once,
  // without waiting for replacements.
  evictNode: (uuid: string) =>
    sendJSON<{ evicted: string[] }>("POST", `/api/v1/nodes/${encodeURIComponent(uuid)}/evict`),
  updateNodeLabels: (uuid: string, labels: Record<string, string>) =>
    sendJSON<Node>("PUT", `/api/v1/nodes/${uuid}/labels`, { labels }),
  renameNode: (uuid: string, name: string) =>
    sendJSON<Node>("PATCH", `/api/v1/nodes/${uuid}/name`, { name }),
  deleteNode: (uuid: string, force = false) =>
    sendJSON<{ status: "deleted"; agent_uninstalled: boolean }>("DELETE", `/api/v1/nodes/${encodeURIComponent(uuid)}${force ? "?force=true" : ""}`),
  upgradeNode: (uuid: string) =>
    sendJSON<{ status: string; run_id?: number }>("POST", `/api/v1/nodes/${uuid}/upgrade`),
  precheckNode: (body: SSHPrecheckRequest) =>
    sendJSON<SSHPrecheckReport>("POST", "/api/v1/nodes/precheck", body),
  installNode: (body: SSHInstallRequest) =>
    sendJSON<SSHInstallReport>("POST", "/api/v1/nodes/install", body),
  nodePullProxy: (uuid: string) =>
    getJSON<NodePullProxy>(`/api/v1/nodes/${uuid}/pull-proxy`),
  nodeNetworks: (uuid: string) =>
    getJSON<NodeNetworks>(`/api/v1/nodes/${uuid}/networks`),
  updateManagementRoles: (nodeUUIDs: string[], allow: NetworkRole[], deny: NetworkRole[]) =>
    sendJSON<{ updated: number }>("PATCH", "/api/v1/nodes/management-roles", { node_uuids: nodeUUIDs, allow, deny }),
  setManagementRoles: (uuid: string, rolesOff: NetworkRole[]) =>
    sendJSON<NodeNetworks>("PUT", `/api/v1/nodes/${uuid}/management-roles`, { roles_off: rolesOff }),
  /** Restarts the node's container runtime daemon so its proxy takes effect (admin). */
  restartNodeRuntime: (uuid: string) =>
    sendJSON<NodeRuntime>("POST", `/api/v1/nodes/${uuid}/runtime/restart`),
  nodeSSH: (uuid: string) =>
    getJSON<SSHCredentialInfo>(`/api/v1/nodes/${uuid}/ssh`),
  putNodeSSH: (
    uuid: string,
    body: {
      host: string;
      port: number;
      user: string;
      auth_mode: SSHAuthMode;
      password?: string;
      private_key_pem?: string;
      probe?: boolean;
    },
  ) => sendJSON<SSHCredentialInfo>("PUT", `/api/v1/nodes/${uuid}/ssh`, body),
  deleteNodeSSH: (uuid: string) =>
    sendJSON<{ status: string }>("DELETE", `/api/v1/nodes/${uuid}/ssh`),
  probeNodeRecovery: (uuid: string) =>
    sendJSON<RecoveryOutcome>("POST", `/api/v1/nodes/${uuid}/recovery/probe`),
};

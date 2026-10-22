import { useState } from "react";
import { api, usePolling, type DeploymentListItem, type Node } from "../../api";

// How many choices one search offers; typing narrows the list on the server.
export const WIZARD_TARGET_PAGE_SIZE = 50;

/**
 * useWizardTargets feeds the benchmark wizard its choices while it is open:
 * deployments that can serve now (state running, which excludes a delete in
 * flight) and online nodes, each one page searched on the server. Fetching
 * every deployment and node did not scale to thousands of them.
 */
export function useWizardTargets(open: boolean) {
  const [deploymentQuery, setDeploymentQuery] = useState("");
  const [nodeQuery, setNodeQuery] = useState("");
  const deployments = usePolling(
    () => (open ? api.deploymentsPage({ page: 1, page_size: WIZARD_TARGET_PAGE_SIZE, q: deploymentQuery, state: ["running"] }) : Promise.resolve(null)),
    open ? 10000 : 0,
    [open, deploymentQuery],
  );
  const nodes = usePolling(
    () => (open ? api.nodesPage({ page: 1, page_size: WIZARD_TARGET_PAGE_SIZE, q: nodeQuery, state: ["online"] }) : Promise.resolve(null)),
    open ? 10000 : 0,
    [open, nodeQuery],
  );
  return {
    deployments: (deployments.data?.items ?? []) as DeploymentListItem[],
    nodes: (nodes.data?.items ?? []) as Node[],
    searchDeployments: setDeploymentQuery,
    searchNodes: setNodeQuery,
  };
}

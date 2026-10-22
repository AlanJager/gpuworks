import { useEffect } from "react";
import type { FormInstance } from "antd";
import type { Node, PartitionCapability } from "../../api";
import { hostOnly } from "./deployHelpers";

// useDeployNodeSync reacts to watched placement facts: a partition-capable
// node owns the backend id, and the distributed draft's leader address feeds
// node_uuid and master_addr.
export default function useDeployNodeSync({
  form,
  mode,
  capability,
  nodes,
  nnodes,
  distributedNodes,
}: {
  form: FormInstance;
  mode: string;
  capability?: PartitionCapability;
  nodes: Node[];
  nnodes: unknown;
  distributedNodes: unknown;
}) {
  const leaderUUID = Array.isArray(distributedNodes)
    ? (distributedNodes as string[])[0]
    : undefined;
  const leaderAddress = nodes.find(
    (node) => node.uuid === leaderUUID,
  )?.management_address;
  useEffect(() => {
    if (
      mode === "manual" &&
      capability?.status === "ready" &&
      form.getFieldValue("vram_limit_gib") &&
      form.getFieldValue("partition_backend") !== capability.backend
    ) {
      form.setFieldValue("partition_backend", capability.backend);
    }
    if (
      Number(nnodes || 1) > 1 &&
      Array.isArray(distributedNodes) &&
      leaderUUID &&
      leaderAddress
    ) {
      form.setFieldsValue({
        node_uuid: leaderUUID,
        master_addr: hostOnly(leaderAddress),
      });
    }
  }, [
    capability?.backend,
    capability?.status,
    distributedNodes,
    form,
    leaderAddress,
    leaderUUID,
    mode,
    nnodes,
  ]);
}

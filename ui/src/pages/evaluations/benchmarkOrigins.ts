import type { Instance } from "../../api";
import { instanceSleepLevel, isFollower } from "../instanceGroups";

// A managed node origin calls one local instance directly. Match the serving
// leader checks in resolveManagedBenchmarkTarget; a PD target uses decode.
// Pending sleep orders also exclude a leader, as they do for gateway routing.
export function benchmarkOriginNodes(instances: Instance[]): string[] {
  return [...new Set(instances.filter((instance) =>
    instance.state === "running" && instanceSleepLevel(instance) === 0 &&
    !instance.headless && !isFollower(instance) && (instance.node_rank ?? 0) === 0 &&
    instance.api_port > 0 && (!instance.role || instance.role === "decode"),
  ).map((instance) => instance.node_uuid))];
}

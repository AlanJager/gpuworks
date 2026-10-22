import type { TFunction } from "i18next";
import type { Deployment, Node } from "../api";
import { gpuHealthStatus, gpuOwned } from "../components/GPUHealth";
import { nodeDisplayName } from "../nodeDisplay";
import { nodeConnection } from "../nodes/connectionState";
import type { PageSubject } from "./AssistantContext";

// failedShown bounds the failed instances a deployment offers the panel; a
// deployment with dozens failing fails for one reason, and a few say it.
const failedShown = 5;

/** deploymentSubjects is what a deployment's detail offers the assistant, attention first. */
export function deploymentSubjects(dep: Deployment, t: TFunction<"ai">): PageSubject[] {
  const waiting = dep.pd ? 0 : dep.placement_status?.unscheduled ?? 0;
  const out: PageSubject[] = [{
    subject: { kind: "deployment", id: dep.id, label: dep.name },
    tasks: waiting > 0 ? ["deployment.unscheduled"] : [],
    attention: waiting > 0 ? t("attention.unscheduled", { count: waiting }) : undefined,
  }];
  for (const inst of (dep.instances ?? []).filter((i) => i.state === "failed").slice(0, failedShown)) {
    out.push({ subject: { kind: "instance", uuid: inst.uuid, label: inst.uuid.slice(0, 8) }, tasks: ["instance.diagnose"], attention: t("attention.failed") });
  }
  return out;
}

/** nodeSubjects is what a node's detail offers the assistant. */
export function nodeSubjects(node: Node, t: TFunction<"ai">): PageSubject[] {
  // Only the node's own cards can be broken; one owned elsewhere is not a
  // fault to report.
  const broken = (node.gpus ?? []).filter((g) => gpuOwned(g) && gpuHealthStatus(g) === "Error").length;
  // A node that is not connected is the first thing to say about it; its
  // cards only matter once the platform can reach them.
  const connection = nodeConnection(node);
  const attention = connection.state === "offline"
    ? t("attention.nodeOffline")
    : connection.state === "connecting"
      ? t("attention.nodeConnecting")
      : broken > 0 ? t("attention.brokenGPUs", { count: broken }) : undefined;
  return [{ subject: { kind: "node", uuid: node.uuid, label: nodeDisplayName(node) }, tasks: ["node.health"], attention }];
}

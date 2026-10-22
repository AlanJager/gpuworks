import type { Node } from "../api";

/**
 * The reasons the server cordons a node whose container runtime cannot hand
 * a container its accelerators (Node.runtime_gpu_blocked). The one place
 * that lists them: the scheduling tag's tooltip, the un-cordon warning and
 * the plugin status all read it. A code outside the list (a newer server)
 * is shown as the code itself rather than as nothing.
 */
export const runtimeGpuBlockedCodes = ["gpu_plugin_missing", "gpu_runtime_unconfigured"] as const;

export type RuntimeGpuBlockedCode = (typeof runtimeGpuBlockedCodes)[number];

type Translate = (key: string, opts?: Record<string, unknown>) => string;

function known(code: string): code is RuntimeGpuBlockedCode {
  return (runtimeGpuBlockedCodes as readonly string[]).includes(code);
}

/** Whether the node's vendor container plugin is what the runtime lacks. */
export function runtimePluginBlocked(node: Pick<Node, "runtime_gpu_blocked">): boolean {
  return node.runtime_gpu_blocked === "gpu_plugin_missing";
}

/**
 * Why the platform cordoned this node, in the reader's own language, or ""
 * when it did not for this reason. The server sends a stable code and the
 * plugin's name; the sentence is the client's.
 */
export function runtimeGpuBlockedReason(t: Translate, node: Pick<Node, "runtime_gpu_blocked" | "runtime_gpu_plugin" | "runtime_driver">): string {
  const code = node.runtime_gpu_blocked;
  if (!code) return "";
  if (!known(code)) return code;
  return t(`scheduling.blocked.reason.${code}`, {
    plugin: node.runtime_gpu_plugin ?? "",
    runtime: node.runtime_driver ?? "",
  });
}

import type { ProviderInstallResponse } from "./api";

// What the operator needs to know after pressing Install, in one word:
//   installed           the agent runs and has connected; nothing to look at
//   installedWithNotes  the same, but some check row warns (the install went through)
//   waitingForAgent     the agent started and has not connected yet
//   waitingForServer    the agent started and the server cannot reach it yet (server_dial)
//   failed              nothing was installed, or the agent did not start
export type InstallOutcome = "installed" | "installedWithNotes" | "waitingForAgent" | "waitingForServer" | "failed";

export interface InstallVerdict {
  outcome: InstallOutcome;
  // The reason, for a failure or a pending connection: the request's own
  // error, or the deciding check's detail.
  detail?: string;
}

// The rows that decide whether the agent is running. Other rows (the precheck
// above all) describe the host and never mean the install did not happen.
const CONNECTION_CHECKS = new Set(["agent_dial", "server_dial"]);

export function installVerdict(result: ProviderInstallResponse | null, requestError: string | null): InstallVerdict | null {
  if (requestError !== null) return { outcome: "failed", detail: requestError };
  if (!result) return null;
  const checks = result.checks ?? [];
  const started = checks.find((check) => check.name === "install agent");
  if (started?.status === "error") return { outcome: "failed", detail: started.detail };
  const connection = checks.find((check) => CONNECTION_CHECKS.has(check.name));
  if (connection?.status === "error") return { outcome: "failed", detail: connection.detail };
  if (connection?.status === "warn") {
    // Who connects decides what to look at: the agent's way out, or the
    // node's firewall in front of the agent's port.
    return { outcome: connection.name === "server_dial" ? "waitingForServer" : "waitingForAgent", detail: connection.detail };
  }
  if (checks.some((check) => check.status === "warn" || check.status === "error")) {
    return { outcome: "installedWithNotes" };
  }
  return { outcome: "installed" };
}

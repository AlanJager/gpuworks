export type InstallConnectMode = "agent_dial" | "server_dial";

export interface InstallCommandOptions {
  baseCommand: string;
  token: string;
  connectMode: InstallConnectMode;
  serverURL: string;
  origin: string;
  installDir?: string;
  // Start the agent under nohup instead of the systemd unit.
  noSystemd?: boolean;
}

function shellQuoteArg(value: string): string {
  return `'${value.replace(/'/g, "'\\''")}'`;
}

// Build the command shown after creating a node, which reads as connecting
// until its agent arrives. Keep this separate
// from the drawer so both URL variants can be covered without a browser test.
export function buildInstallCommand(options: InstallCommandOptions): string {
  const normalizedServerURL = options.serverURL.trim().replace(/\/+$/, "");
  const customControlPlane = options.connectMode === "agent_dial" &&
    normalizedServerURL && normalizedServerURL !== options.origin;
  const command = customControlPlane
    ? `curl -fsSL ${shellQuoteArg(`${normalizedServerURL}/install.sh`)} | sh -s -- --server ${shellQuoteArg(normalizedServerURL)} --token ${shellQuoteArg(options.token)}`
    : options.baseCommand;
  const dir = options.installDir && options.installDir !== "/opt/gpuworks"
    ? ` --install-dir ${shellQuoteArg(options.installDir)}`
    : "";
  return command + dir + (options.noSystemd ? " --no-systemd" : "");
}

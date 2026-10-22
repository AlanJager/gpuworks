import type { ProviderInstallRequest } from "./api";

export interface ProviderInstallFormValues {
  externalID: string;
  serverURL: string;
  sshKeyPEM: string;
  password: string;
  sshConnectionID: string;
  connectMode: "agent_dial" | "server_dial";
  serverDialListen: string;
  serverDialAddress: string;
  allowInstanceRestart?: boolean;
  // Where the agent goes on the instance ("" for the default) and whether it
  // runs without the systemd unit.
  installDir?: string;
  noSystemd?: boolean;
}

export function buildProviderInstallRequest(values: ProviderInstallFormValues): ProviderInstallRequest {
  return {
    external_id: values.externalID,
    // An agent the server dials never contacts the server: no address.
    server_url: values.connectMode === "agent_dial" ? values.serverURL.trim() || undefined : undefined,
    ssh_key_pem: values.sshKeyPEM.trim() || undefined,
    password: values.password || undefined,
    ssh_connection_id: values.sshConnectionID || "auto",
    connect_mode: values.connectMode,
    server_dial_listen: values.connectMode === "server_dial" ? values.serverDialListen.trim() || undefined : undefined,
    server_dial_address: values.connectMode === "server_dial" ? values.serverDialAddress.trim() || undefined : undefined,
    allow_instance_restart: values.allowInstanceRestart ? true : undefined,
    install_dir: values.installDir?.trim() || undefined,
    no_systemd: values.noSystemd ? true : undefined,
  };
}

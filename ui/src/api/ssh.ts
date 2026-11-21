import type { CheckStatus } from "../checkStatus";

// How the server logs in to a node over SSH. platform_key is the server's own
// key pair (see PlatformSSHKey); it needs nothing from the operator once the
// node authorized its public key.
export type SSHAuthMode = "platform_key" | "password" | "private_key" | "keyboard_interactive";

export interface SSHCredentialInfo {
  host: string;
  port: number;
  user: string;
  has_password: boolean;
  has_private_key: boolean;
  auth_mode: SSHAuthMode;
  probe?: SSHProbeResult;
  // Authorizing the platform key with the password or key just saved.
  platform_key?: CheckResult;
}

export interface PlatformSSHKey {
  public_key: string;
  fingerprint: string;
  created_at: string;
  authorize_command: string;
}

export interface SSHProbeResult {
  reachable: boolean;
  agent_running: boolean;
  agent_installed: boolean;
  // Server an active gpuworks-agent unit on the host was installed for, when
  // it is not this node's server.
  foreign_server_url?: string;
  detail?: string;
}

export interface RecoveryOutcome {
  node_uuid: string;
  phase?: string;
  action: string;
  probe: SSHProbeResult;
  recovered: boolean;
  /** Why the round did nothing (RECOVERY_SKIPPED_*); skipped_reason is its English text. */
  skipped_code?: string;
  skipped_reason?: string;
  /** Names error, the same code recorded on the node. */
  error_code?: string;
  error?: string;
}

export interface SSHPrecheckRequest {
  target: string;
  auth_mode?: SSHAuthMode;
  password?: string;
  private_key_pem?: string;
  server_url?: string;
  // Directory the agent will be installed into; the precheck inspects the
  // filesystem behind it. Omitted means the install default.
  install_dir?: string;
  connect_mode?: "agent_dial" | "server_dial";
  // server_dial only: where the agent will listen and the address the server
  // dials it at; the precheck probes that address from the server.
  server_dial_listen?: string;
  host_address?: string;
}

/** One onboarding check (ADR 0067): which check (id), how it came out (one
 * of the five check statuses), the outcome as a code with params (rendered
 * from apiError.<code>, message being the English fallback), and what a
 * command or provider answered (evidence). */
export interface CheckResult {
  id: string;
  status: CheckStatus;
  code?: string;
  params?: Record<string, unknown>;
  message?: string;
  evidence?: string;
}

export interface SSHPrecheckReport {
  target: string;
  user: string;
  host: string;
  port: number;
  checks: CheckResult[];
}

export interface SSHInstallRequest {
  target: string;
  auth_mode?: SSHAuthMode;
  name?: string;
  password?: string;
  private_key_pem?: string;
  server_url?: string;
  install_dir?: string;
  connect_mode?: "agent_dial" | "server_dial";
  udocker_bundle?: string;
  server_dial_listen?: string;
  // Start a Linux agent under nohup instead of the gpuworks-agent systemd
  // unit; omitted means systemd. macOS always uses launchd.
  no_systemd?: boolean;
  // Where the server reaches the host for server_dial (default: the SSH host).
  host_address?: string;
  // Confirms replacing a gpuworks agent already on the host; without it the
  // server answers 409 NODE_INSTALL_HOST_ALREADY_NODE or NODE_INSTALL_EXISTING_AGENT(_UNKNOWN/_SELF).
  replace_existing_agent?: boolean;
}

export interface SSHInstallReport {
  node_uuid: string;
  target: string;
  user: string;
  host: string;
  port: number;
  arch: string;
  binary_name: string;
  install_dir: string;
  server_url?: string;
  server_dial_address?: string;
  auth_used: SSHAuthMode | "instance_password" | "";
  platform_key_authorized: boolean;
  checks: CheckResult[];
}

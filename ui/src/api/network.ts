export interface NetworkConfig {
  http_proxy?: string;
  https_proxy?: string;
  /** Hosts and networks the proxy is not used for; also written into dockerd's proxy. */
  no_proxy?: string;
}

/**
 * What a docker node's agent observes about the proxy dockerd pulls through,
 * and what it can do about it. Absent for runtimes that apply the node proxy
 * on every pull and for offline nodes.
 */
export interface RuntimeProxy {
  http_proxy: string;
  https_proxy: string;
  no_proxy: string;
  /** What dockerd starts with, as systemd has the unit loaded (masked; empty when unknown). */
  configured_http_proxy: string;
  configured_https_proxy: string;
  /** What the platform wrote into the dockerd drop-in file. */
  written_http_proxy: string;
  written_https_proxy: string;
  written_no_proxy: string;
  /** dockerd keeps containers running across its own restart. */
  live_restore: boolean;
  /** /etc/docker/daemon.json also sets proxies, and those win over the drop-in. */
  daemon_json_proxy: boolean;
  systemd: boolean;
  privilege: "root" | "sudo" | "none" | string;
  /** dockerd already uses the proxy the platform set for the node; with none set, the proxy it is configured with. */
  applied: boolean;
  error?: string;
}

export interface NodeRuntime {
  driver: string;
  proxy?: RuntimeProxy;
}

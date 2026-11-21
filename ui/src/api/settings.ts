// ServerListen is the control plane's listen configuration (server.yaml).
// Agent and gateway are empty when they inherit the console addresses.
export interface ServerListen {
  console: string[];
  agent: string[];
  gateway: string[];
  running: { addr: string; functions: string[] }[];
  restart_pending: boolean;
  config_path: string;
  editable: boolean;
  overridden: boolean;
  restart_mode: "self" | "command";
  restart_command?: string;
}

// SecuritySettings are the platform-wide security switches; only an
// administrator can change them.
/**
 * Branding is how the console presents itself. An empty field means the
 * built-in default, rendered in the viewer's language.
 */
export interface Branding {
  product_name: string;
  console_title: string;
  login_description: string;
  /** Base64 data: URL (PNG, JPEG, WebP or SVG), or "" for the built-in mark. */
  logo: string;
}

export interface SecuritySettings {
  /** Lets the playground HTML preview make network requests; off by default. */
  playground_preview_network: boolean;
  /**
   * Accepts /v1 requests that present no API key, anonymously; off by default.
   * A request that does present a key is still verified either way.
   */
  inference_open_access: boolean;
}

export interface ServerListenUpdate {
  console: string[];
  agent: string[];
  gateway: string[];
}

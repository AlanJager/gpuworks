/** A platform API token: authenticates the management API and MCP as its user. */
export interface PlatformToken {
  id: number;
  name: string;
  prefix: string;
  scope: "read" | "write";
  created_at: string;
  expires_at: string | null;
  last_used_at: string | null;
}

/** Whether the MCP service serves and how to reach it. */
export interface MCPStatus {
  enabled: boolean;
  path: string;
  protocol_version: string;
  tools: string[];
}

export interface ApiKey {
  /** Stable usage-analytics identifier; older servers omit it. */
  id?: number;
  /** Recorded owner; zero is a historical unassigned key. Older servers omit it. */
  user_id?: number;
  name: string;
  prefix: string;
  last_used?: string;
  created_at: string;
  /** When the key was revoked; a revoked key no longer authenticates. */
  revoked_at?: string;
  /** The key may call every deployment; otherwise only `deployments`. */
  all_deployments: boolean;
  /** Empty when all_deployments, or when every granted deployment is gone. */
  deployments: ApiKeyDeployment[];
  /** The key's request priority binding (PUT /apikeys/{prefix}/priority). */
  priority: ApiKeyPriority;
}

/**
 * An API key's request priority binding: the class its requests run at ("" is
 * the default class), the highest class a request may ask for ("" is its own
 * class), and how a request may choose (one of PriorityClasses.policies).
 */
export interface ApiKeyPriority {
  class: string;
  ceiling: string;
  policy: string;
}

/** A request priority class (GET /priority-classes): larger value is more important. */
export interface PriorityClass {
  name: string;
  display_name: string;
  description: string;
  value: number;
  builtin: boolean;
  default: boolean;
  /** Whether DELETE would take it: decided by the server (built in, the default, or named by a live key). */
  deletable: boolean;
  created_at: string;
}

/** GET /priority-classes: the classes, the bounds of a value and the key policies. */
export interface PriorityClasses {
  items: PriorityClass[];
  min_value: number;
  max_value: number;
  policies: string[];
  default_policy: string;
  /** The policies a ceiling applies to; a binding of another policy has none. */
  ceiling_policies: string[];
}

/** A deployment a restricted API key may call. */
export interface ApiKeyDeployment {
  id: number;
  name: string;
  served_name: string;
}

/** Which deployments an API key may call (POST /apikeys, PUT /apikeys/{prefix}/scope). */
export interface ApiKeyScope {
  all_deployments: boolean;
  deployment_ids: number[];
}

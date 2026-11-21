import type { LocalizedText } from "../i18n/localizedText";

export interface EngineParamOption {
  value: string;
  label: LocalizedText;
}

export interface RoutingPolicyOption {
  value: "least_request" | "prefix_affinity" | "failover";
  label: LocalizedText;
  description: LocalizedText;
}

export interface EngineParamSpec {
  key: string;
  flag: string;
  // The engine's negative form of a boolean parameter, when it has one
  // (vLLM's --no-enable-prefix-caching, SGLang's --disable-radix-cache). The
  // server owns both polarities; the form only submits the value.
  flag_off?: string;
  // Accepted spellings mapped to the canonical value, so a catalog that writes
  // `--dtype half` and a form that submits `float16` are one value. The server
  // resolves them; the form never rewrites a value of its own.
  aliases?: Record<string, string>;
  label: LocalizedText;
  type: string;
  control?:
    | "segmented"
    | "slider"
    | "switch"
    | "number"
    | "select"
    | "combobox"
    | "text"
    | "json"
    | "path"
    | "tier"
    | string;
  group?:
    | "model"
    | "multimodal"
    | "performance"
    | "speculative"
    | "performance_advanced"
    | string;
  // Where the deploy form shows the parameter ("quick" surface, "expert" fold,
  // "hidden" never) and the order inside that surface. The dictionary owns the
  // layout; the form holds no key lists of its own.
  layout?: "quick" | "expert" | "hidden" | string;
  order?: number;
  enum?: string[];
  options?: EngineParamOption[];
  // The dictionary's default for this key. A boolean without one is
  // three-state on the form: "auto" leaves it unstated, so no flag is sent and
  // the engine keeps its own default.
  default?: unknown;
  min?: number;
  max?: number;
  step?: number;
  placeholder?: LocalizedText;
  help?: LocalizedText;
  engines?: string[];
  advanced?: boolean;
  automatic?: boolean;
  /** Dictionary key whose enabled tier makes this parameter effective. */
  depends_on?: string;
  multimodal_only?: boolean;
}

export interface EngineProfile {
  name: string;
  default_image: string;
  health_path: string;
  shm_bytes: number;
  sleep: { supported: boolean; levels?: string[] };
  metric_prefix: string;
  param_dict?: Record<string, string>;
  param_specs?: EngineParamSpec[];
  quick_form_keys?: string[];
  // The lowest kernel release on which this engine's KV disk tier starts;
  // absent when the engine states no floor. Clients compare it against the
  // kernel a node reports instead of carrying a release number of their own.
  kv_disk_min_kernel?: string;
  // The PD modes this engine can run, with each mode's rules (ADR 0028's
  // pdmode catalog); empty or absent when the engine has none. The deploy
  // form and the scale dialog read modes, policy options, defaults and
  // limits from here (pages/deploy/deployPd.ts) instead of restating them.
  pd_modes?: PdModeProfile[];
  // The KV offload backends a deployment can select besides the engine's own
  // tiers (ADR 0069); absent when there are none. The deploy form renders one
  // card per entry from these fields alone and holds no backend of its own.
  kv_backends?: KVBackendProfile[];
  multi_node_supported?: boolean;
}

// KVBackendProfile is one selectable KV offload backend: its id (the value of
// the kv_offload_backend parameter), its product name, the native parameters
// it replaces while selected, and the PD roles that keep it.
export interface KVBackendProfile {
  id: string;
  label: string;
  replaces: string[];
  pd_roles: string[];
}

// PdModeProfile is one PD mode as the server's catalog states it.
export interface PdModeProfile {
  name: string;
  // Accelerator vendors the mode runs on.
  vendors: string[];
  roles: PdRoleName[];
  // Roles a running deployment may shrink to no instances.
  scalable_to_zero: PdRoleName[];
  // Direction rule between the two roles' TP; the server enforces it.
  tp_rule: string;
  max_units_per_role: number;
  // Allowed policy values, the default first. kv_load_failures is empty when
  // the mode's engine has no such setting.
  fallbacks: string[];
  kv_load_failures: string[];
  unverified?: boolean;
  // The first request after a start compiles kernels and takes minutes.
  first_request_compiles?: boolean;
}

// PdRoleName is the fixed pair every PD mode declares today (ADR 0028); the
// mode catalog is the only place new role names could come from.
export type PdRoleName = "prefill" | "decode";

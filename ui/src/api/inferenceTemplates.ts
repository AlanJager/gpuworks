import type { ListPage } from "./http";
import type { PdRoleName } from "./engines";
import type { ProductWarning } from "./scheduling";

export type InferenceTemplatePage = ListPage<InferenceTemplate>;
export interface InferenceTemplateIndex {
  hardware?: DetectedTemplateChip[];
  items: InferenceTemplateIndexItem[];
}
export interface InferenceTemplateOrganizations {
  items: Array<{ organization: string; models: number }>;
}
export interface InferenceTemplateIndexItem {
  id: number;
  model_id: string;
  display_title?: string;
  source: "upstream" | "user" | string;
  engines: string[];
  scenarios: string[];
  profiles: InferenceProfileSummary[];
}
export interface InferenceProfileSummary {
  detected_compatible?: boolean;
  source?: "upstream" | "user" | string;
  name: string;
  display_name_zh_cn?: string;
  display_name_en?: string;
  scenario?: string;
  engine: string;
  image?: string;
  quantization?: string;
  weight_bytes?: number;
  weight_repo_ids?: string[];
  gpus: GPUPredicate;
}
export interface InferenceTemplateDetail extends InferenceTemplate {
  can_delete: boolean;
}
export interface InferenceTemplate {
  detected_support?: { preferred_engine: string; warnings: Array<{ reason: string; quantization?: string; alternative_available: boolean }> };
  id: number;
  model_id: string;
  display_title?: string;
  parameter_count?: string;
  architecture?: string;
  input_modalities?: Array<"text" | "image" | "audio" | "video" | string>;
  readme_zh_cn?: string;
  readme_en?: string;
  readme_zh_cn_content?: string;
  readme_en_content?: string;
  context_length?: number;
  release_date?: string;
  source: "upstream" | "user" | string;
  unsupported?: InferenceTemplateUnsupported[];
  specs: InferenceSpec[];
}
/** One role of a PD-only spec's layout: one instance's shape and how many instances to start with (server-filled defaults). */
export interface InferencePdRoleShape {
  tp: number;
  pp: number;
  /** Nodes ONE instance spans: leader plus followers. */
  nodes: number;
  gpus_per_node: number;
  /** Instances of the role a deployment starts with. */
  units: number;
}
export interface InferenceSpec {
  id: number;
  /** Set on a spec deployed only as PD, in that mode. */
  pd_mode?: string;
  /** The layout a PD-only spec is written for, by role; only with pd_mode. */
  pd_roles?: Partial<Record<PdRoleName, InferencePdRoleShape>>;
  /** The shell script a deployment of the spec runs as its manual command. */
  manual_command?: string;
  source: "upstream" | "user" | string;
  name: string;
  /** Human-written names; the page shows the one for its locale (specDisplayName). */
  display_name_zh_cn?: string;
  display_name_en?: string;
  scenario?: string;
  engine: string;
  image: string;
  /** Further references the template offers for pulling `image`, preferred first. */
  image_sources?: string[];
  quantization?: string;
  weight_sources: WeightSource[];
  weight_bytes?: number;
  runtime_weight_bytes?: number;
  /** /dev/shm size in bytes the spec's engine needs; absent when the spec states none. */
  shm_bytes?: number;
  /** The spec's container starts with unlimited locked memory (--ulimit memlock=-1:-1); absent when false. */
  memlock_unlimited?: boolean;
  /** The instance start timeout in seconds the spec states; absent when none. */
  start_timeout_sec?: number;
  /** Extra docker run options the spec's engine needs, applied before the deployment's own docker_args. */
  docker_args?: string[];
  /** Writable host directories the platform keeps for a deployment of the spec on every node (ADR 0077); absent when none. */
  dirs?: Array<{ name: string; mount_path: string; limit_bytes: number; retain: string }>;
  gpus: GPUPredicate;
  args?: string[];
  env?: Record<string, string>;
  pre_start_argv?: string[];
  pre_start_script?: string;
  assets?: Array<{
    file: string;
    sha256: string;
    size_bytes?: number;
    unpack_to?: string;
  }>;
  entrypoint?: string[];
  // Keys whose default this spec's launch leaves to the engine.
  engine_decides_param_keys?: string[];
  placement?: { language: string; resolver: string };
  api?: import("../inferenceApi").InferenceAPIContract;
  components?: Array<{
    name: string;
    role: string;
    model_id?: string;
    mount_path: string;
    weight_bytes: number;
    weights: { quantization?: string; sources: WeightSource[] };
  }>;
}
export interface WeightSource {
  provider: string;
  repo_id: string;
  revision?: string;
  subfolder?: string;
}
export interface GPUPredicate {
  vendor: string;
  cc?: string;
  name?: string;
  arch_id?: string;
}
export interface InferenceTemplateUnsupported {
  gpus: GPUPredicate;
  quantization?: string;
  reason: string;
}
export interface InferenceTemplateSummary {
  deployable: number;
  models: number;
}
export interface InferenceTemplateChip {
  vendor: string;
  name: string;
  cc?: number;
  category: string;
  architecture: string;
  arch_id?: string;
}
export interface DetectedTemplateChip {
  vendor: string;
  name: string;
  cc?: string;
  arch_id?: string;
}

/** The result of applying an official inference template catalog. */
export interface OfficialInferenceCatalogResult {
  models: number;
  package_digest: string;
  version: string;
  /** What the server left out because it cannot read it; absent when nothing was. */
  skipped?: OfficialInferenceCatalogSkipped[];
}

/** A model, or one spec of it when `spec` is set, that needs a newer server. */
export interface OfficialInferenceCatalogSkipped {
  model_id: string;
  spec?: string;
  reason: string;
}

// One finding about a template document (POST /inference-templates/import
// and /evaluate): `error` is the server's English text, shown only for a code
// the console has no localized text for.
export interface InferenceTemplateIssue {
  code: string;
  error: string;
  params?: Record<string, unknown>;
}

// The import answer; `warnings` is absent when there are none.
// POST /deployments/{id}/catalog-template: a running deployment written as a
// flat catalog template (docs/how-to/save-deployment-as-template.md). The
// server tests the cheap operations before it writes the YAML; notes are the
// facts the YAML comments state, by stable code.
/** One operation the export tested, or did not (status skip, code says why). */
export interface CatalogTemplateCheck {
  id: string;
  operation: string;
  status: string;
  code: string;
  params?: Record<string, unknown>;
  message: string;
  /** What the gateway answered instead of an answer; external text. */
  evidence?: string;
}

export interface CatalogTemplateExport {
  model_id: string;
  spec_name: string;
  yaml: string;
  verified: boolean;
  saveable: boolean;
  checks: CatalogTemplateCheck[];
  notes: ProductWarning[];
  excluded_env: string[];
  evidence: string;
}

export interface InferenceTemplateImportResult {
  models: number;
  package_digest?: string;
  warnings?: InferenceTemplateIssue[];
}

// The inference catalog import and the remote sync take the same two shapes: a
// flat YAML document (JSON is YAML too), or a .gpuworks-catalog.tar.gz package,
// which a template with assets or documentation needs. The server tells them
// apart by the gzip magic; the content type only labels the upload.
export const inferenceCatalogFileAccept = ".yaml,.yml,.json,.tgz,.gz,application/yaml,text/yaml,application/json,application/gzip,application/x-gzip";

export function isInferenceCatalogPackage(file: File): boolean {
  return /\.(tgz|gz)$/i.test(file.name) || file.type === "application/gzip" || file.type === "application/x-gzip";
}

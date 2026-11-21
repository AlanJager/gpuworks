// --- AI helper -------------------------------------------------------------
/** AIPreset is a preset's stable name; the server's dictionary lists them. */
export type AIPreset = string;
/**
 * AIProxyMode is how calls to an AI service leave the server: the management
 * node's outbound proxy, no proxy, or a proxy URL of the service's own.
 */
export type AIProxyMode = "management" | "direct" | "custom";
export interface AIConfig {
  id: number;
  name: string;
  preset: string;
  base_url: string;
  model: string;
  is_default: boolean;
  proxy_mode: AIProxyMode;
  /** The custom proxy with its login masked; present only for custom. */
  proxy_url?: string;
  image_input: boolean;
}
export interface AIConfigInput {
  name: string;
  preset: AIPreset;
  base_url: string;
  api_key: string;
  model: string;
  is_default: boolean;
  proxy_mode: AIProxyMode;
  proxy_url?: string;
  image_input: boolean;
}
/**
 * AIConfigPatch changes a stored service in place. Only the fields present
 * change; api_key is left out to keep the stored key, which the API never
 * sends back, and an empty proxy_url keeps the stored proxy while the mode
 * stays custom.
 */
export interface AIConfigPatch {
  name?: string;
  preset?: AIPreset;
  base_url?: string;
  api_key?: string;
  model?: string;
  is_default?: boolean;
  proxy_mode?: AIProxyMode;
  proxy_url?: string;
  image_input?: boolean;
}
/**
 * AIPresetDictionary is the server's preset table: each provider's address,
 * starting model and image switch, in the order offered, ending with custom;
 * and the proxy modes a service may use.
 */
export interface AIPresetDictionary {
  presets: { name: AIPreset; base_url: string; model: string; image_input: boolean }[];
  proxy_modes: AIProxyMode[];
}
/**
 * AIConfigDeleteResult answers a delete. promoted_id names the service that took
 * over as the default when the deleted one was in use, and is absent when
 * nothing changed hands: the console says which service the assistant moved to
 * instead of letting the data-egress notice change host unannounced.
 */
export interface AIConfigDeleteResult {
  status: string;
  promoted_id?: number;
}
/**
 * AIConfigTest asks whether an endpoint answers before it is saved. id names
 * the stored service an edit started from; with an empty api_key its stored
 * key is used, while base_url still matches the stored address.
 */
export interface AIConfigTest {
  id?: number;
  base_url: string;
  api_key?: string;
  model: string;
  proxy_mode?: AIProxyMode;
  proxy_url?: string;
  /** Also send a small image once text works, to tell whether the model accepts images. */
  check_image?: boolean;
}
/**
 * AIConfigTestResult is what a connection test established. outcome is a
 * stable code the console words itself: ok, connection (DNS, dial or
 * timeout), blocked (refused by the outbound policy), auth (401/403), path (a
 * 404 that names no model: the wrong address), model (the endpoint does not
 * serve that model) or http (any other answer). detail is the endpoint's own
 * words, shortened and with the key removed.
 */
export interface AIConfigTestResult {
  outcome: "ok" | "connection" | "blocked" | "auth" | "path" | "model" | "http";
  status?: number;
  detail?: string;
  base_url: string;
  latency_ms: number;
  /** The image check, when asked for and the text call succeeded. */
  image?: "accepted" | "ignored" | "rejected" | "unknown";
  image_status?: number;
  image_detail?: string;
}
/**
 * One assistant task about a subject named by reference (instance_uuid,
 * model_id, deployment_id or node_uuid); the server gathers its context.
 * Task "ask" is a free question. A follow-up carries the thread's earlier
 * turns in history.
 */
export interface AIAskRequest {
  task: "instance.diagnose" | "model.fit" | "deployment.unscheduled" | "node.health" | "ask";
  instance_uuid?: string;
  model_id?: number;
  deployment_id?: number;
  node_uuid?: string;
  question?: string;
  /** data: URLs sent with the question, within AIAvailability.images. */
  images?: string[];
  history?: AIHistoryMessage[];
  locale: string;
}
export interface AIStep {
  tool: string;
  args: string;
  ms: number;
  error?: string;
}
export interface AIAnswer {
  answer: string;
  steps: AIStep[];
  calls: number;
  usage: { prompt_tokens: number; completion_tokens: number };
}
/** A known failure the instance's logs match; the client renders code. */
export interface AIFinding {
  code: string;
  evidence: { line: string }[];
}
export interface AIFitRow {
  vendor: string;
  name: string;
  memory_bytes: number;
  nodes: number;
  cards: number;
  /** Broken cards (the UI's Error health level); cards counts the usable ones. */
  unusable: number;
  max_cards_per_node: number;
  dtype_supported: boolean;
  dtype_note?: string;
  /** Largest group one node links by NVLink or another fast interconnect; 1 = PCIe only, 0 = unknown. */
  fast_link_cards: number;
  fits: boolean;
  tp: number;
  per_gpu_bytes: number;
  usable_bytes: number;
  headroom_bytes: number;
}
/** One earlier turn of a thread; only a user turn carries images. */
export interface AIHistoryMessage {
  role: "user" | "assistant";
  content: string;
  images?: string[];
}
/** AIImageLimit bounds the images of one ask (GET /api/v1/ai/available). */
export interface AIImageLimit {
  per_message: number;
  per_request: number;
  max_bytes: number;
  max_side: number;
}
/**
 * AIAvailability says whether the assistant answers and whether the AI
 * service in use takes images, with the limits to keep when it does.
 */
export interface AIAvailability {
  available: boolean;
  image_input: boolean;
  images?: AIImageLimit;
}
/** One event of a streamed /ai/ask answer (Accept: text/event-stream). */
export type AIStreamEvent =
  | { type: "step"; id: number; tool: string; args: string; status: "running" | "done" | "error"; ms: number; error?: string }
  | { type: "text" | "reasoning"; delta: string }
  | { type: "done"; calls: number; usage: { prompt_tokens: number; completion_tokens: number }; first_text_ms: number; total_ms: number }
  | { type: "error"; error: string; code?: string };
export interface AIModelFit {
  model_id: number;
  weight_bytes: number;
  dtype?: string;
  gpu_memory_utilization: number;
  rows: AIFitRow[];
}

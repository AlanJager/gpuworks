import type { LocalizedText } from "../../i18n/localizedText";
import type { InferenceAPIOperation } from "../../inferenceApi";
export type { AsyncLifecycle, ExampleRequest, InferenceAPIOperation as PlaygroundOperation, OperationExample, OperationType } from "../../inferenceApi";

export type Capability = "chat" | "transcription" | "embedding" | "rerank";
export interface PlaygroundModel {
  id: string;
  label: string;
  capability: Capability;
  operations: InferenceAPIOperation[];
  modalities?: string[];
  input_modalities?: string[];
  context_length?: number;
  sleeping: boolean;
  deployment_id?: number;
  template_id?: number;
  template_spec_source?: string;
  template_spec_name?: string;
  external?: boolean;
  public_data_plane: boolean;
}

export interface ParameterDefinition {
  name: string;
  label: LocalizedText;
  kind: "number" | "integer" | "string";
  minimum?: number;
  maximum?: number;
  step?: number;
  default: unknown;
}

// StreamOptionDefinition is one boolean stream_options field the Playground
// adds to a streamed chat request; requires names an option that must be on
// for this one to take effect.
export interface StreamOptionDefinition {
  name: string;
  label: LocalizedText;
  description: LocalizedText;
  default: boolean;
  requires?: string;
}

export interface ExtraParameterPreset {
  label: LocalizedText;
  value: Record<string, unknown>;
}

export interface PlaygroundCatalog {
  models: PlaygroundModel[];
  parameters: ParameterDefinition[];
  extra_parameter_presets: ExtraParameterPreset[];
  stream_options?: StreamOptionDefinition[];
  /** The policy HTML previews of replies run under (platform security settings). */
  html_preview: { allow_network: boolean };
}

export interface ParameterState {
  enabled: Record<string, boolean>;
  values: Record<string, unknown>;
  systemPrompt: string;
  extraJSON: string;
  // Absent in sessions saved before stream options were switchable; every
  // option then takes its default.
  streamOptions?: Record<string, boolean>;
}

export interface Attachment {
  id: string;
  name: string;
  type: "image" | "audio";
  dataURL: string;
}

export interface Usage {
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
  prompt_tokens_details?: { cached_tokens?: number };
  [key: string]: unknown;
}

export interface PerformanceMetrics {
  ttftMS?: number;
  tpotMS?: number;
  tps?: number;
  prefillTPS?: number;
  durationMS: number;
  outputTokens?: number;
  // Token counts were estimated from chunks: the endpoint reported no usage.
  approximate?: boolean;
}

export interface ChatMessage {
  id: string;
  role: "user" | "assistant";
  content: string;
  reasoning?: string;
  attachments?: Attachment[];
  usage?: Usage;
  metrics?: PerformanceMetrics;
  error?: string;
  streaming?: boolean;
  // The model that answered (assistant messages).
  model?: string;
  // The page instance generating a streaming reply; a reply left streaming by
  // a page that is gone was interrupted.
  owner?: string;
  // Generation was cut off by a reload or a closed tab.
  interrupted?: boolean;
}

export interface CompletionMessage {
  role: "system" | "user" | "assistant";
  content: unknown;
}

import type { NodeRuntime } from "./network";
import type { NodeRequirementsMap } from "./acquisition";

export interface PullSourceRule {
  id: number;
  position: number;
  /** "url" for a site rule (the match has an http(s) scheme), "image" for a registry rule. */
  kind: "image" | "url";
  match: string;
  endpoints: string[];
  allow_upstream: boolean;
  /** The management node reads the site through this rule too (model metadata); site rules only. */
  control_plane: boolean;
  note: string;
  node_requirements: NodeRequirementsMap;
  matched_nodes: number;
}

/** The addresses the console's browser may read a source's model metadata at; empty when the rules forbid them all. */
export interface MetadataSources {
  source: string;
  bases: string[];
}

/** A draft site rule and the node asked whether it reaches its addresses (nothing is saved). */
export interface PullSourceRuleTestRequest {
  match: string;
  endpoints: string[];
  allow_upstream: boolean;
  node_uuid: string;
  repo?: string;
}

export type SourceProbeStatus = "ok" | "auth_required" | "http_error" | "unreachable";

export interface SourceProbeOutcome {
  base: string;
  status: SourceProbeStatus;
  code?: number;
  latency_ms: number;
  error?: string;
}

/** The node's answer, address by address, in the order it would try them. */
export interface SourceRuleTestResult {
  source: string;
  repo: string;
  node_uuid: string;
  node_name: string;
  outcomes: SourceProbeOutcome[];
}

export interface PullSourceRuleRequest {
  match: string;
  endpoints: string[];
  allow_upstream: boolean;
  control_plane?: boolean;
  note: string;
  node_requirements: NodeRequirementsMap;
}

/** One ordered proxy entry; a node uses the first entry whose node requirements it meets. Addresses never carry credentials. */
export interface PullProxy {
  id: number;
  position: number;
  http_proxy: string;
  https_proxy: string;
  no_proxy: string;
  username: string;
  has_password: boolean;
  /** The management node goes out through this proxy too (model metadata, logos, catalog URLs, login tests). */
  control_plane: boolean;
  note: string;
  node_requirements: NodeRequirementsMap;
  matched_nodes: number;
}

/** password: omitted keeps the stored one on update, "" removes it, a value sets it. */
export interface PullProxyRequest {
  http_proxy: string;
  https_proxy: string;
  no_proxy: string;
  username: string;
  password?: string;
  control_plane: boolean;
  note: string;
  node_requirements: NodeRequirementsMap;
}

/** The proxy entry one node uses (null: none, the agent uses its own environment) and its container runtime's proxy status. */
export interface NodePullProxy {
  proxy: PullProxy | null;
  runtime?: NodeRuntime;
}

export interface SyncGroup {
  id: number;
  name: string;
  node_requirements: NodeRequirementsMap;
  /** 0 = unlimited. */
  rate_limit_kib_s: number;
  matched_nodes: number;
}

export interface SyncGroupRequest {
  name: string;
  node_requirements: NodeRequirementsMap;
  rate_limit_kib_s: number;
}

/**
 * Credential is one login per host. An empty username is a model site's token
 * (sent as a Bearer token over https to that exact host); otherwise it is a
 * registry login (user and password).
 */
export interface Credential {
  id: number;
  host: string;
  username: string;
  has_secret: boolean;
}

export interface CredentialRequest {
  host: string;
  username: string;
  /** On an update an empty secret keeps the stored one. */
  secret: string;
}

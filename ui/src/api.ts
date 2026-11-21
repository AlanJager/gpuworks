// API client + shared types. Types mirror internal/server/api/dto.
//
// This file only re-exports. The types and helpers of each domain live in
// api/<domain>.ts, the endpoints of the `api` object in api/client/<domain>.ts,
// and the fetch wrappers in api/http.ts.
export type { CheckStatus } from "./checkStatus";
export type * from "./peerTransferTypes";
export type * from "./benchmarkEvidenceTypes";
export type * from "./networkTypes";
export type * from "./storageTypes";
export type * from "./agentUpgradesTypes";
export type { MonitorRange } from "./monitorRange";
export * from "./api/http";
export * from "./api/connectionTest";
export * from "./api/server";
export * from "./api/auth";
export * from "./api/settings";
export * from "./api/updates";
export * from "./api/platform";
export * from "./api/cardHealth";
export * from "./api/gpus";
export * from "./api/nodes";
export * from "./api/ssh";
export * from "./api/doctor";
export * from "./api/network";
export * from "./api/storage";
export * from "./api/engines";
export * from "./api/models";
export * from "./api/images";
export * from "./api/pullSources";
export * from "./api/acquisition";
export * from "./api/inferenceTemplates";
export * from "./api/scheduling";
export * from "./api/evaluate";
export * from "./api/wake";
export * from "./api/instances";
export * from "./api/deployments";
export * from "./api/dgpus";
export * from "./api/dashboard";
export * from "./api/kube";
export * from "./api/providers";
export * from "./api/apikeys";
export * from "./api/dataRetention";
export * from "./api/ai";
export * from "./api/benchmarks";
export * from "./api/metrics";
export * from "./api/logs";
export * from "./api/monitor";
export * from "./api/events";
export * from "./api/polling";
export { api } from "./api/client";

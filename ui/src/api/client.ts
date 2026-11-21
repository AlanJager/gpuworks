import { serverApi } from "./client/server";
import { authApi } from "./client/auth";
import { settingsApi } from "./client/settings";
import { updatesApi } from "./client/updates";
import { agentsApi } from "./client/agents";
import { gpusApi } from "./client/gpus";
import { nodesApi } from "./client/nodes";
import { sshApi } from "./client/ssh";
import { networkApi } from "./client/network";
import { storageApi } from "./client/storage";
import { enginesApi } from "./client/engines";
import { modelsApi } from "./client/models";
import { imagesApi } from "./client/images";
import { pullSourcesApi } from "./client/pullSources";
import { inferenceTemplatesApi } from "./client/inferenceTemplates";
import { evaluateApi } from "./client/evaluate";
import { instancesApi } from "./client/instances";
import { deploymentsApi } from "./client/deployments";
import { dashboardApi } from "./client/dashboard";
import { kubeApi } from "./client/kube";
import { slurmApi } from "./client/slurm";
import { providersApi } from "./client/providers";
import { apikeysApi } from "./client/apikeys";
import { dataRetentionApi } from "./client/dataRetention";
import { aiApi } from "./client/ai";
import { benchmarksApi } from "./client/benchmarks";
import { logsApi } from "./client/logs";
import { monitorApi } from "./client/monitor";
import { telemetryApi } from "./client/telemetry";
import { diagnosticsApi } from "./client/diagnostics";

// The one `api` object every page calls. Each domain keeps its endpoints in
// client/<domain>.ts; a new endpoint goes into its domain's file, not here.
export const api = {
  ...serverApi,
  ...authApi,
  ...settingsApi,
  ...updatesApi,
  ...agentsApi,
  ...gpusApi,
  ...nodesApi,
  ...sshApi,
  ...networkApi,
  ...storageApi,
  ...enginesApi,
  ...modelsApi,
  ...imagesApi,
  ...pullSourcesApi,
  ...inferenceTemplatesApi,
  ...evaluateApi,
  ...instancesApi,
  ...deploymentsApi,
  ...dashboardApi,
  ...kubeApi,
  ...slurmApi,
  ...providersApi,
  ...apikeysApi,
  ...dataRetentionApi,
  ...aiApi,
  ...benchmarksApi,
  ...logsApi,
  ...monitorApi,
  ...telemetryApi,
  ...diagnosticsApi,
};

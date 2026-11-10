import { useEffect, useState } from "react";
import type { TFunction } from "i18next";
import { api, type KubeCluster, type KubeDefaults } from "./api";

/**
 * The cluster form's defaults, limits and vendors, which the server owns
 * (GET /api/v1/kube/defaults). null until they arrive; error says why they
 * did not, and the form then sends only what was typed.
 */
export function useKubeDefaults(enabled: boolean): { defaults: KubeDefaults | null; error: Error | null } {
  const [state, setState] = useState<{ defaults: KubeDefaults | null; error: Error | null }>({ defaults: null, error: null });
  useEffect(() => {
    if (!enabled) return;
    let alive = true;
    api.kubeDefaults()
      .then((defaults) => { if (alive) setState({ defaults, error: null }); })
      .catch((error: Error) => { if (alive) setState((previous) => ({ defaults: previous.defaults, error })); });
    return () => { alive = false; };
  }, [enabled]);
  return state;
}

/** The kubectl commands that show why a cluster's agents did not join. */
export function kubeTroubleshootCommands(namespace: string): string {
  const ns = /^[a-z0-9-]+$/.test(namespace) ? namespace : `'${namespace}'`;
  return [
    `kubectl -n ${ns} get daemonset,pods -l app.kubernetes.io/name=gpuworks-agent -o wide`,
    `kubectl -n ${ns} describe daemonset gpuworks-agent`,
    `kubectl -n ${ns} logs -l app.kubernetes.io/name=gpuworks-agent --tail=50`,
  ].join("\n");
}

/** How long a borrowed card may sit idle before it goes back, as the list shows it. */
export function cardReturnText(t: TFunction<"kubernetes">, idleReturnSeconds: number): string {
  if (idleReturnSeconds < 0) return t("returnPolicy.keep");
  const duration =
    idleReturnSeconds > 0 && idleReturnSeconds % 3600 === 0 ? t("duration.hours", { count: idleReturnSeconds / 3600 })
    : idleReturnSeconds > 0 && idleReturnSeconds % 60 === 0 ? t("duration.minutes", { count: idleReturnSeconds / 60 })
    : t("duration.seconds", { count: idleReturnSeconds });
  return t("returnPolicy.afterIdle", { duration });
}

/**
 * The clusters' names by id, for a list of nodes that says which cluster each
 * Kubernetes node is in. One request when the page opens: a failure (a viewer
 * without the right, an old server) leaves the map empty and the nodes show
 * their Kubernetes node name alone.
 */
export function useKubeClusterNames(): Map<number, string> {
  const [names, setNames] = useState<Map<number, string>>(() => new Map());
  useEffect(() => {
    let alive = true;
    api.kubeClusters()
      .then((clusters) => { if (alive) setNames(new Map((clusters.items ?? []).map((cluster: KubeCluster) => [cluster.id, cluster.name]))); })
      .catch(() => {});
    return () => { alive = false; };
  }, []);
  return names;
}

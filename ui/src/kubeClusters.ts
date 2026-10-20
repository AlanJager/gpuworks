import { useEffect, useState } from "react";
import type { TFunction } from "i18next";
import { api, type KubeCluster, type KubeVendor } from "./api";

/** The idle_return_seconds value that keeps a borrowed card for good. */
export const KEEP_BORROWED_CARDS = -1;

/** The accelerator vendors a cluster can be connected for, the first being the server's default. */
export const KUBE_VENDORS: readonly KubeVendor[] = ["nvidia", "ascend", "hygon", "ppu"];

/**
 * The defaults the server applies to an omitted field (docs/reference/api.md, Kubernetes).
 * The containerd socket has none: empty means the agents detect it themselves.
 */
export const KUBE_DEFAULTS = {
  vendor: "nvidia",
  namespace: "gpuworks-system",
  stateDir: "/opt/gpuworks",
  runtimeClass: "nvidia",
  enginePorts: "40000-42767",
  idleReturnSeconds: 60,
} as const;

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

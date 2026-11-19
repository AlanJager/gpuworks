import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type ProviderHPCCluster, type ProviderHPCClusterList } from "../../api";
import { apiErrorText } from "../../apiError";

/**
 * A cloud account's HPC clusters (Alibaba Cloud E-HPC), read live from the
 * cloud the way its managed Kubernetes clusters are (useAccountClusters), so a
 * cluster with no GPU node yet, or one before a sync, is listed and can be
 * connected. Nothing is read while `enabled` is false (a kind without HPC
 * clusters). null until the cloud is first read; a later read keeps the
 * clusters listed meanwhile, so a drawer or dialog open on one stays open.
 * `listing` says how far the cloud let the account be listed (a missing RAM
 * permission or an account that never activated E-HPC is no error: the
 * clusters are just empty), and which regions could not be read. The server
 * answers its last listing at once and reads the cloud again behind it
 * (refreshing): the hook then asks again shortly, until the new one is in.
 * load({ fresh: true }) waits for a new read, as after a sync.
 */
/** How soon a listing the server is still refreshing is asked for again. */
export const HPC_REFRESH_POLL_MS = 2000;

export function useAccountHPCClusters({ providerId, enabled }: { providerId: number; enabled: boolean }) {
  const { t: tCommon } = useTranslation("common");
  const [clusters, setClusters] = useState<ProviderHPCCluster[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [listing, setListing] = useState<Omit<ProviderHPCClusterList, "items" | "total"> | null>(null);
  // Only the latest read lands: an earlier one answering late is dropped.
  const readRef = useRef(0);
  const againRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  const load = useCallback((options: { fresh?: boolean } = {}) => {
    if (!enabled) return;
    clearTimeout(againRef.current);
    const read = ++readRef.current;
    api.providerHPCClusters(providerId, options.fresh)
      .then((list) => {
        if (read !== readRef.current) return;
        setClusters(list.items ?? []);
        setListing({ access: list.access, permission: list.permission, code: list.code, partial: list.partial, failed_regions: list.failed_regions ?? [], listed_at: list.listed_at, refreshing: list.refreshing });
        setLoadError(null);
        if (list.refreshing) againRef.current = setTimeout(() => { if (read === readRef.current) load(); }, HPC_REFRESH_POLL_MS);
      })
      .catch((e: unknown) => { if (read !== readRef.current) return; setClusters((current) => current ?? []); setLoadError(apiErrorText(tCommon, e)); });
  }, [providerId, enabled, tCommon]);
  useEffect(() => {
    load();
    // A read still out when the account changes or the list goes away is not applied.
    return () => { readRef.current++; clearTimeout(againRef.current); };
  }, [load]);

  return { clusters, loadError, listing, load };
}

export type AccountHPCClusters = ReturnType<typeof useAccountHPCClusters>;

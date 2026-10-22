import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import type { Storage } from "../../storageTypes";

function completeNodes(storage: Storage): boolean {
  return storage.nodes.total === 0 || storage.nodes.all?.length === storage.nodes.total;
}

/** Summary rows omit node facts. Fetch them separately without resetting form drafts. */
export function useStorageCachePreview(open: boolean, storage?: Storage | null) {
  const { t } = useTranslation("storage");
  const [detail, setDetail] = useState<Storage | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [attempt, setAttempt] = useState(0);
  const id = storage?.id;
  const needsDetail = !!storage && storage.type === "nfs_cache" && !completeNodes(storage);
  useEffect(() => {
    let alive = true;
    setDetail(null);
    setError(null);
    if (open && needsDetail && id !== undefined) {
      void api.storage(id, true).then((result) => {
        if (!completeNodes(result)) throw new Error(t("cacheDraft.previewIncomplete"));
        if (alive) setDetail(result);
      }).catch((failure) => { if (alive) setError(failure instanceof Error ? failure : new Error(String(failure))); });
    }
    return () => { alive = false; };
  }, [open, id, needsDetail, attempt, t]);
  const current = needsDetail ? detail?.id === id ? detail : null : storage ?? null;
  return { storage: current, loading: open && needsDetail && !current && !error, error, retry: () => setAttempt((value) => value + 1) };
}

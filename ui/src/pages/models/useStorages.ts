import { useCallback, useEffect, useState } from "react";
import { api } from "../../api";
import { canWriteStorage, type Storage } from "../../storageTypes";

export interface StoragesState {
  storages: Storage[];
  loading: boolean;
  error: Error | null;
  reload: () => void;
}

/** The storages a model dialog offers; loaded each time `active` turns on, never cached across openings. */
export function useStorages(active: boolean): StoragesState {
  const [storages, setStorages] = useState<Storage[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const [round, setRound] = useState(0);
  useEffect(() => {
    if (!active) return;
    let alive = true;
    setLoading(true);
    setError(null);
    api.storages()
      .then((list) => { if (alive) setStorages(list.items); })
      .catch((e) => { if (alive) setError(e instanceof Error ? e : new Error(String(e))); })
      .finally(() => { if (alive) setLoading(false); });
    return () => { alive = false; };
  }, [active, round]);
  const reload = useCallback(() => setRound((n) => n + 1), []);
  return { storages, loading, error, reload };
}

/** The storage dialogs preselect: the default one if it takes writes, else the first that does. */
export function preferredWritable(storages: Storage[]): Storage | undefined {
  const writable = storages.filter(canWriteStorage);
  return writable.find((s) => s.is_default) ?? writable[0];
}

export interface LocalStorageChoice {
  /** True when there is more than one enabled local storage, so the person has to say which. */
  choose: boolean;
  options: Storage[];
  defaultId?: number;
}

/**
 * The enabled local storages a registered path can belong to. With one (or
 * none, or while unknown) there is nothing to choose and requests stay as they
 * were before storages existed.
 */
export function useLocalStorageChoice(active: boolean): LocalStorageChoice {
  const { storages } = useStorages(active);
  const options = storages.filter((s) => s.type === "local" && s.enabled);
  const preferred = options.find((s) => s.is_default) ?? options[0];
  return { choose: options.length >= 2, options, defaultId: preferred?.id };
}

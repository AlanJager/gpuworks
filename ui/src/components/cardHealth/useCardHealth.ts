import { useCallback, useEffect, useState } from "react";
import { api, type CardHealthChecks, type CardHealthRecords, type CardRef } from "../../api";
import { apiErrorText } from "../../apiError";
import type { TFunction } from "i18next";

// How often the table is read again while it is open: it comes from the
// server's memory, so this costs no agent anything.
const refreshEvery = 30_000;

/** The node's (or one card's) check table, read again every half minute. */
export function useCardHealthChecks(nodeUUID: string, card: CardRef | undefined, tCommon: TFunction) {
  const [data, setData] = useState<CardHealthChecks>();
  const [error, setError] = useState<string>();
  const pcieBDF = card?.pcie_bdf;
  const uuid = card?.uuid;
  const load = useCallback(() => {
    api.nodeHealthChecks(nodeUUID, pcieBDF || uuid ? { pcie_bdf: pcieBDF, uuid } : undefined).then(
      (d) => {
        setData(d);
        setError(undefined);
      },
      (err) => setError(apiErrorText(tCommon, err)),
    );
  }, [nodeUUID, pcieBDF, uuid, tCommon]);
  useEffect(() => {
    setData(undefined);
    load();
    const timer = setInterval(load, refreshEvery);
    return () => clearInterval(timer);
  }, [load]);
  return { data, error, reload: load };
}

/** A page of the node's (or one card's, one check's) failure records. */
export function useCardHealthRecords(nodeUUID: string, card: CardRef | undefined, check: string | undefined, page: number, tCommon: TFunction) {
  const [data, setData] = useState<CardHealthRecords>();
  const [error, setError] = useState<string>();
  const pcieBDF = card?.pcie_bdf;
  const uuid = card?.uuid;
  const load = useCallback(() => {
    api.nodeHealthRecords(nodeUUID, { card: pcieBDF || uuid ? { pcie_bdf: pcieBDF, uuid } : undefined, check, page }).then(
      (d) => {
        setData(d);
        setError(undefined);
      },
      (err) => setError(apiErrorText(tCommon, err)),
    );
  }, [nodeUUID, pcieBDF, uuid, check, page, tCommon]);
  useEffect(() => {
    setData(undefined);
    load();
  }, [load]);
  return { data, error, reload: load };
}

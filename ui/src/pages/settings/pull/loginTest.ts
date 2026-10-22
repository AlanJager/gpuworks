import { useCallback, useState } from "react";
import { api } from "../../../api";
import { apiErrorText } from "../../../apiError";
import { useTranslation } from "react-i18next";

export type LoginTestState = { state: "testing" } | { state: "ok" } | { state: "failed"; text: string };

/**
 * useLoginTests runs "log in to the registry or check the site token from the server with the stored
 * credential" and keeps the latest outcome per credential for this page view
 * (the server stores no test history). A rejected login and an unreachable
 * registry read differently; both come back as product errors.
 */
export function useLoginTests() {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const [results, setResults] = useState<Record<number, LoginTestState>>({});
  const test = useCallback(async (id: number): Promise<LoginTestState> => {
    setResults((current) => ({ ...current, [id]: { state: "testing" } }));
    let outcome: LoginTestState;
    try {
      const reply = await api.testCredential(id);
      outcome = reply?.ok === false ? { state: "failed", text: reply.message || t("pull.credentials.testFailed") } : { state: "ok" };
    } catch (err) {
      outcome = { state: "failed", text: apiErrorText(tCommon, err) };
    }
    setResults((current) => ({ ...current, [id]: outcome }));
    return outcome;
  }, [t, tCommon]);
  return { results, test };
}

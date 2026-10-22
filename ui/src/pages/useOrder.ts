import { useState } from "react";
import { useTranslation } from "react-i18next";
import { apiErrorText } from "../apiError";

// useOrder runs an order the operator gave (start, restart, scale, sleep...)
// with the busy flag its button shows. run resolves to null when the order
// went through and to the reason, as the user's locale words it, when the
// server refused it; callers decide where that reason is shown.
export function useOrder() {
  const { t: tCommon } = useTranslation("common");
  const [busy, setBusy] = useState(false);
  const run = async (order: () => Promise<unknown>): Promise<string | null> => {
    setBusy(true);
    try {
      await order();
      return null;
    } catch (e) {
      return apiErrorText(tCommon, e);
    } finally {
      setBusy(false);
    }
  };
  return { busy, run };
}

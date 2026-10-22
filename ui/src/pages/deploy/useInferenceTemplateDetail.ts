import { message } from "antd";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { api, type InferenceTemplate } from "../../api";

export function useInferenceTemplateDetail(modelID?: string) {
  const { t } = useTranslation("deploy");
  const [loaded, setLoaded] = useState<InferenceTemplate>();
  useEffect(() => {
    let cancelled = false;
    if (!modelID) {
      setLoaded(undefined);
      return () => {
        cancelled = true;
      };
    }
    void api
      .inferenceTemplate(modelID)
      .then((template) => {
        if (!cancelled) setLoaded(template);
      })
      .catch((error) => {
        if (!cancelled) {
          setLoaded(undefined);
          message.error(
            t("templateRecommendations.loadFailed", {
              detail: (error as Error).message,
            }),
          );
        }
      });
    return () => {
      cancelled = true;
    };
  }, [modelID, t]);
  return loaded?.model_id === modelID ? loaded : undefined;
}

import { Alert } from "antd";
import { useTranslation } from "react-i18next";
import type { InferenceAPIOperation } from "../../inferenceApi";
import type { Model } from "../../api";
import { operationTypesOf } from "../../modelInference";

// A model that declares no inference operation deploys into a deployment with
// an empty contract (inferenceapi.EffectiveContract derives no default): the
// Playground lists it nowhere and the health check has nothing to ask. Say so
// before it is deployed, while the model's operations can still be set on the
// Models page. A deployment from a template gives a model without operations
// the template spec's (docs/reference/inference-catalog-spec.md), [] included,
// so a spec that declares some is enough.
export default function DeployNoOperationsNotice({
  model,
  templateOperations,
}: {
  model?: Model | null;
  templateOperations?: Pick<InferenceAPIOperation, "type">[];
}) {
  const { t } = useTranslation("deploy");
  if (!model || operationTypesOf(model.operations).length > 0 || operationTypesOf(templateOperations).length > 0) return null;
  return (
    <Alert
      className="deploy-no-operations"
      type="warning"
      showIcon
      message={t("noOperations.title")}
      description={t("noOperations.description")}
    />
  );
}

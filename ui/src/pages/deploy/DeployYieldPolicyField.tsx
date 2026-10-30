import { useContext } from "react";
import { Form, Select } from "antd";
import { useTranslation } from "react-i18next";
import { api, usePolling, type YieldPolicyDictionary, type YieldPolicyName } from "../../api";
import { draftOfPolicy, paramsOfValue, type YieldParams } from "../yieldPolicy/model";
import { DeployEditContext } from "./DeployEditContext";

// DeployYieldPolicyField picks the yield policy a new deployment starts with,
// which decides how it shares its GPUs with the others on them (never by
// default). Choosing one fills in the policy's default parameters, which the
// server's dictionary holds. It shows only for a deployment the server accepts
// a policy other than never for - sleepable, single-node, not PD - and only
// when creating: an existing deployment's policy is changed on its scheduling
// policy card, where its parameters can be tuned too.
export default function DeployYieldPolicyField() {
  const form = Form.useFormInstance();
  const { mode } = useContext(DeployEditContext);
  const watch = { form, preserve: true } as const;
  const sleepable = Boolean(Form.useWatch("sleepable", watch));
  const nnodes = Number(Form.useWatch("nnodes", watch) || 1);
  const nodeMode = String(Form.useWatch("node_selection_mode", watch) || "");
  if (mode === "edit" || !sleepable || nnodes > 1 || nodeMode === "pd") return null;
  return <YieldPolicyItem />;
}

// The form holds the policy with its parameters (undefined for never); the
// select shows its name.
function PolicySelect({ value, onChange, dictionary, loading }: {
  value?: YieldParams;
  onChange?: (value: YieldParams | undefined) => void;
  dictionary: YieldPolicyDictionary | null;
  loading: boolean;
}) {
  const { t } = useTranslation("deploy");
  const { t: tPolicy } = useTranslation("deployments");
  const held = paramsOfValue(value)?.policy ?? "never";
  const names: YieldPolicyName[] = dictionary ? dictionary.policies.map((entry) => entry.policy) : [...new Set<YieldPolicyName>(["never", held])];
  return (
    <Select<YieldPolicyName>
      aria-label={t("yieldPolicy.label")}
      loading={loading}
      value={held}
      optionLabelProp="short"
      onChange={(policy) => {
        if (policy === "never" || !dictionary) onChange?.(undefined);
        else onChange?.(paramsOfValue(draftOfPolicy(dictionary, policy)));
      }}
      options={names.map((policy) => ({
        value: policy,
        short: tPolicy(`yieldPolicy.policy.name.${policy}`),
        label: `${tPolicy(`yieldPolicy.policy.name.${policy}`)} - ${tPolicy(`yieldPolicy.policy.description.${policy}`)}`,
      }))}
    />
  );
}

function YieldPolicyItem() {
  const { t } = useTranslation("deploy");
  const dictionary = usePolling(api.yieldPolicies, 0);
  return (
    <Form.Item
      label={t("yieldPolicy.label")}
      name="yield_policy"
      extra={dictionary.error ? t("yieldPolicy.loadFailed", { detail: dictionary.error.message }) : t("yieldPolicy.help")}
      validateStatus={dictionary.error ? "warning" : undefined}
    >
      <PolicySelect dictionary={dictionary.data} loading={dictionary.loading} />
    </Form.Item>
  );
}

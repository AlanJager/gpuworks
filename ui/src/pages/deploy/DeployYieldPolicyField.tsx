import { useContext } from "react";
import { Form, Select } from "antd";
import { useTranslation } from "react-i18next";
import { api, usePolling, type Eligibility, type YieldPolicyDictionary, type YieldPolicyName } from "../../api";
import { draftOfPolicy, paramsOfValue, type YieldParams } from "../yieldPolicy/model";
import { DeployEditContext } from "./DeployEditContext";

// DeployYieldPolicyField picks the yield policy a new deployment starts with,
// which decides how it shares its GPUs with the others on them (never by
// default). Choosing one fills in the policy's default parameters, which the
// server's dictionary holds. Whether the draft may have a policy other than
// never is the server's answer (the evaluation's yield_policy_eligible): the
// field shows where it may, and where the form still holds such a policy the
// draft no longer allows, with the server's reason, so it can be set back to
// never - the create would refuse it. It shows only when creating: an existing
// deployment's policy is changed on its scheduling policy card, where its
// parameters can be tuned too.
export default function DeployYieldPolicyField({ eligibility }: { eligibility?: Eligibility }) {
  const form = Form.useFormInstance();
  const { mode } = useContext(DeployEditContext);
  const held = paramsOfValue(Form.useWatch("yield_policy", { form, preserve: true }))?.policy ?? "never";
  if (mode === "edit") return null;
  if (eligibility?.ok) return <YieldPolicyItem allowed />;
  if (held !== "never") return <YieldPolicyItem allowed={false} reason={eligibility?.reason} />;
  return null;
}

// The form holds the policy with its parameters (undefined for never); the
// select shows its name.
function PolicySelect({ value, onChange, dictionary, loading, allowed }: {
  value?: YieldParams;
  onChange?: (value: YieldParams | undefined) => void;
  dictionary: YieldPolicyDictionary | null;
  loading: boolean;
  allowed: boolean;
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
        disabled: !allowed && policy !== "never",
      }))}
    />
  );
}

function YieldPolicyItem({ allowed, reason }: { allowed: boolean; reason?: string }) {
  const { t } = useTranslation("deploy");
  const { t: tCommon } = useTranslation("common");
  const engine = String(Form.useWatch("engine", { preserve: true }) ?? "");
  const dictionary = usePolling(api.yieldPolicies, 0);
  const refusal = !allowed && reason ? tCommon(`apiError.${reason}`, { engine, defaultValue: reason }) : "";
  return (
    <Form.Item
      label={t("yieldPolicy.label")}
      name="yield_policy"
      extra={refusal || (dictionary.error ? t("yieldPolicy.loadFailed", { detail: dictionary.error.message }) : t("yieldPolicy.help"))}
      validateStatus={refusal ? "error" : dictionary.error ? "warning" : undefined}
    >
      <PolicySelect dictionary={dictionary.data} loading={dictionary.loading} allowed={allowed} />
    </Form.Item>
  );
}

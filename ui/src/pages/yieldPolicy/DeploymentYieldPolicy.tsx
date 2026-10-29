import { useMemo, useState } from "react";
import { Alert, Button, Select, Skeleton, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, usePolling, type Deployment, type YieldPolicyDictionary, type YieldPolicyName } from "../../api";
import { APIError, apiErrorText } from "../../apiError";
import YieldPolicyFields from "./YieldPolicyFields";
import {
  canYield,
  draftEqual,
  draftErrors,
  draftOfParams,
  draftOfPolicy,
  modifiedFields,
  requestOfDraft,
  savedPolicy,
  applicable,
  type YieldDraft,
} from "./model";

// A refusal in the user's language. The server's invalid-field refusal is worded
// with the editor's own field names and the range; the rest go through the
// shared product error texts.
function refusalText(
  t: (key: string, options?: Record<string, unknown>) => string,
  tCommon: (key: string, options: Record<string, unknown>) => string,
  error: unknown,
): string {
  if (error instanceof APIError && error.code === "YIELD_POLICY_INVALID") {
    const params = error.params ?? {};
    const reason = String(params.reason ?? "");
    const field = String(params.field ?? "");
    if (["invalid_value", "out_of_range", "required"].includes(reason)) {
      return t(`yieldPolicy.error.${reason}`, {
        field: t(`yieldPolicy.field.${field}`, { defaultValue: field }),
        minimum: params.minimum ?? "",
        maximum: params.maximum ?? "",
      });
    }
  }
  return apiErrorText(tCommon, error);
}

// YieldPolicyEditor sets how the deployment shares its GPUs with the others on
// them: one yield policy and the parameters that apply to it. Which policies
// and parameters there are, their defaults and bounds come from the server's
// dictionary, which the editor waits for. The scheduling policy card keys it
// by what is saved, so it restarts from that. A policy other than never needs a
// sleepable single-node, non-PD deployment; a deployment that kept one it can
// no longer have can still go back (choose never).
export default function YieldPolicyEditor({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const dictionary = usePolling(api.yieldPolicies, 0);
  return (
    <section aria-label={t("yieldPolicy.title")}>
      <Typography.Paragraph type="secondary" style={{ margin: 0, fontSize: 12 }}>{t("yieldPolicy.hint")}</Typography.Paragraph>
      {!dep.sleepable && <Alert style={{ marginTop: 8 }} type="info" showIcon message={t("yieldPolicy.notSleepable")} />}
      {dep.sleepable && !canYield(dep) && <Alert style={{ marginTop: 8 }} type="info" showIcon message={t("yieldPolicy.singleNodeOnly")} />}
      {dictionary.data ? (
        <YieldPolicyForm dep={dep} dictionary={dictionary.data} onChanged={onChanged} />
      ) : dictionary.error ? (
        <Alert
          style={{ marginTop: 12 }}
          type="error"
          showIcon
          message={t("yieldPolicy.dictionary.failed", { detail: dictionary.error.message })}
          action={<Button size="small" onClick={() => void dictionary.refetch()}>{t("yieldPolicy.dictionary.retry")}</Button>}
        />
      ) : (
        <div style={{ marginTop: 12 }} role="status" aria-label={t("yieldPolicy.dictionary.loading")}>
          <Skeleton active paragraph={{ rows: 3 }} />
        </div>
      )}
    </section>
  );
}

function YieldPolicyForm({ dep, dictionary, onChanged }: { dep: Deployment; dictionary: YieldPolicyDictionary; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const saved = useMemo(() => savedPolicy(dep), [dep]);
  const [draft, setDraft] = useState<YieldDraft>(() => draftOfParams(saved));
  const [busy, setBusy] = useState(false);
  const [refused, setRefused] = useState<string | null>(null);
  const yields = canYield(dep);

  const applies = applicable(dictionary, draft);
  const errors = draftErrors(dictionary, draft);
  const modified = modifiedFields(dictionary, draft);
  const dirty = !draftEqual(dictionary, draft, saved);
  const change = (next: YieldDraft) => {
    setRefused(null);
    setDraft(next);
  };

  const save = async () => {
    setBusy(true);
    setRefused(null);
    let answer;
    try {
      answer = await api.setYieldPolicy(dep.id, requestOfDraft(dictionary, draft));
    } catch (error) {
      setRefused(refusalText(t, tCommon, error));
      return;
    } finally {
      setBusy(false);
    }
    message.success(t("yieldPolicy.saved"));
    if (answer.idle_neighbours && answer.idle_neighbours.length > 0) {
      message.warning({ content: t("yieldPolicy.idleNeighbours", { names: answer.idle_neighbours.join(t("yieldPolicy.nameSeparator")) }), duration: 12 });
    }
    onChanged();
  };

  return (
    <>
      <div role="group" aria-label={t("yieldPolicy.policyLabel")} style={{ marginTop: 12 }}>
        <Typography.Text strong style={{ display: "block", marginBottom: 4 }}>{t("yieldPolicy.policyLabel")}</Typography.Text>
        <Select<YieldPolicyName>
          size="small"
          value={draft.policy}
          disabled={busy}
          aria-label={t("yieldPolicy.policyLabel")}
          optionLabelProp="short"
          virtual={false}
          style={{ width: 360, maxWidth: "100%" }}
          onChange={(policy) => change(draftOfPolicy(dictionary, policy))}
          options={dictionary.policies.map(({ policy }) => ({
            value: policy,
            short: t(`yieldPolicy.policy.name.${policy}`),
            label: `${t(`yieldPolicy.policy.name.${policy}`)} - ${t(`yieldPolicy.policy.description.${policy}`)}`,
            disabled: !yields && policy !== "never",
          }))}
        />
        <Typography.Text type="secondary" style={{ display: "block", fontSize: 12, marginTop: 4 }}>{t(`yieldPolicy.policy.description.${draft.policy}`)}</Typography.Text>
      </div>

      <YieldPolicyFields dictionary={dictionary} draft={draft} errors={errors} modified={modified} busy={busy} onChange={change} />

      {refused && <Alert style={{ marginTop: 12 }} type="error" showIcon message={t("yieldPolicy.saveFailed", { detail: refused })} />}
      <div style={{ marginTop: 12 }}>
        <Button size="small" type="primary" loading={busy} disabled={!dirty || errors.length > 0} onClick={() => void save()}>
          {t("automation.save")}
        </Button>
        {applies.size > 0 && (
          <Button size="small" style={{ marginLeft: 8 }} disabled={busy || modified.size === 0} onClick={() => change(draftOfPolicy(dictionary, draft.policy))}>
            {t("yieldPolicy.restoreDefaults")}
          </Button>
        )}
      </div>
    </>
  );
}

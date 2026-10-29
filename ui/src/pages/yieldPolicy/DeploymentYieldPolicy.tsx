import { useMemo, useState } from "react";
import { Alert, Button, Radio, Select, Space, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type Deployment, type YieldPolicyName, type YieldPreset } from "../../api";
import { APIError, apiErrorText } from "../../apiError";
import YieldPolicyFields from "./YieldPolicyFields";
import {
  applicable,
  canAutoSleep,
  canYield,
  draftEqual,
  draftErrors,
  draftOfParams,
  draftOfPolicy,
  draftOfPreset,
  presetOfDraft,
  requestOfDraft,
  savedPolicy,
  yieldPolicyNames,
  yieldPresets,
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
// them and whether it sleeps by itself when idle: a preset (the service level),
// or a yield policy with the parameters that apply to it. The scheduling
// policy card keys it by what is saved, so it restarts from that. A policy
// other than never needs a sleepable single-node, non-PD deployment, and
// automatic sleep a sleepable one; a deployment that kept what it can no longer
// have can still go back (turn it off, choose never).
export default function YieldPolicyEditor({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const saved = useMemo(() => draftOfParams(savedPolicy(dep)), [dep]);
  const [draft, setDraft] = useState<YieldDraft>(saved);
  const [busy, setBusy] = useState(false);
  const [refused, setRefused] = useState<string | null>(null);
  const yields = canYield(dep);
  const sleeps = canAutoSleep(dep);

  const preset = presetOfDraft(draft);
  const use = applicable(draft);
  const errors = draftErrors(draft);
  const dirty = !draftEqual(draft, saved);
  const change = (patch: Partial<YieldDraft>) => {
    setRefused(null);
    setDraft((current) => ({ ...current, ...patch }));
  };

  const save = async () => {
    setBusy(true);
    setRefused(null);
    try {
      await api.setYieldPolicy(dep.id, requestOfDraft(draft));
    } catch (error) {
      setRefused(refusalText(t, tCommon, error));
      return;
    } finally {
      setBusy(false);
    }
    message.success(t("yieldPolicy.saved"));
    onChanged();
  };

  return (
    <section aria-label={t("yieldPolicy.title")}>
      <Typography.Paragraph type="secondary" style={{ margin: 0, fontSize: 12 }}>{t("yieldPolicy.hint")}</Typography.Paragraph>
      {!sleeps && <Alert style={{ marginTop: 8 }} type="info" showIcon message={t("yieldPolicy.notSleepable")} />}
      {sleeps && !yields && <Alert style={{ marginTop: 8 }} type="info" showIcon message={t("yieldPolicy.singleNodeOnly")} />}

      <Typography.Text strong style={{ display: "block", marginTop: 12, marginBottom: 4 }}>{t("yieldPolicy.presetLabel")}</Typography.Text>
      <div role="radiogroup" aria-label={t("yieldPolicy.presetLabel")}>
        <Radio.Group
          value={preset}
          disabled={busy}
          onChange={(event) => {
            // Custom is a result, never a choice: its radio is always disabled.
            const next = event.target.value as YieldPreset;
            change({ ...draftOfPreset(next) });
          }}
        >
          <Space wrap size={[16, 4]}>
            {yieldPresets.map((value) => (
              <Radio key={value} value={value} disabled={busy || (!yields && value !== "resident")}>{t(`yieldPolicy.preset.name.${value}`)}</Radio>
            ))}
            <Radio value="custom" disabled>{t("yieldPolicy.preset.name.custom")}</Radio>
          </Space>
        </Radio.Group>
      </div>
      <Typography.Text type="secondary" style={{ display: "block", fontSize: 12, marginTop: 4 }}>{t(`yieldPolicy.preset.description.${preset}`)}</Typography.Text>

      <div role="group" aria-label={t("yieldPolicy.policyLabel")} style={{ marginTop: 12 }}>
        <Typography.Text strong style={{ display: "block", marginBottom: 4 }}>{t("yieldPolicy.policyLabel")}</Typography.Text>
        <Select<YieldPolicyName>
          size="small"
          value={draft.policy}
          disabled={busy}
          aria-label={t("yieldPolicy.policyLabel")}
          optionLabelProp="short"
          style={{ width: 360, maxWidth: "100%" }}
          onChange={(policy) => {
            setRefused(null);
            setDraft((current) => draftOfPolicy(policy, current.auto_sleep));
          }}
          options={yieldPolicyNames.map((value) => ({
            value,
            short: t(`yieldPolicy.policy.name.${value}`),
            label: `${t(`yieldPolicy.policy.name.${value}`)} - ${t(`yieldPolicy.policy.description.${value}`)}`,
            disabled: !yields && value !== "never",
          }))}
        />
      </div>

      <YieldPolicyFields draft={draft} use={use} errors={errors} busy={busy} canAutoSleep={sleeps} onChange={change} />

      {refused && <Alert style={{ marginTop: 12 }} type="error" showIcon message={t("yieldPolicy.saveFailed", { detail: refused })} />}
      <div style={{ marginTop: 12 }}>
        <Button size="small" type="primary" loading={busy} disabled={!dirty || errors.length > 0} onClick={() => void save()}>
          {t("automation.save")}
        </Button>
      </div>
    </section>
  );
}

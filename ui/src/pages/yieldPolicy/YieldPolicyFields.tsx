import type { ReactNode } from "react";
import { InputNumber, Radio, Select, Space, Tag, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { YieldParamName, YieldPolicyDictionary, YieldSleepLevel, YieldUnfinished } from "../../api";
import {
  type NumericField,
  type YieldDraft,
  type YieldFieldError,
  applicable,
  boundOf,
  chooseUnfinished,
} from "./model";

// Props the parameter controls share: the dictionary (what applies, the
// bounds, the choices), the working copy, the refusals the server would give,
// which parameters differ from the policy's defaults, whether the form is
// locked (a save is running) and the change handler.
export interface FieldsProps {
  dictionary: YieldPolicyDictionary;
  draft: YieldDraft;
  errors: YieldFieldError[];
  modified: Set<YieldParamName>;
  busy: boolean;
  onChange: (next: YieldDraft) => void;
}

const rowStyle = { marginTop: 12 } as const;

// FieldRow is one labelled parameter: its name (with a tag when it differs from
// the default), the control, and a line of help below.
function FieldRow({ label, help, modified, children }: { label: string; help?: ReactNode; modified?: boolean; children: ReactNode }) {
  const { t } = useTranslation("deployments");
  return (
    <div role="group" aria-label={label} style={rowStyle}>
      <div style={{ marginBottom: 4 }}>
        <Typography.Text strong>{label}</Typography.Text>
        {modified && <Tag color="orange" style={{ marginLeft: 8 }}>{t("yieldPolicy.modified")}</Tag>}
      </div>
      {children}
      {help && <Typography.Paragraph type="secondary" style={{ margin: "4px 0 0", fontSize: 12 }}>{help}</Typography.Paragraph>}
    </div>
  );
}

// SecondsInput is a number of seconds held to the dictionary's bounds for the
// field; a value outside them is flagged, not clamped, so the operator sees
// what they typed.
function SecondsInput({ field, props, label }: { field: NumericField; props: FieldsProps; label: string }) {
  const { t } = useTranslation("deployments");
  const bound = boundOf(props.dictionary, props.draft, field);
  const error = props.errors.find((item) => item.field === field);
  return (
    <>
      <InputNumber
        size="small"
        precision={0}
        value={props.draft[field]}
        status={error ? "error" : undefined}
        disabled={props.busy}
        aria-label={label}
        aria-invalid={error ? true : undefined}
        addonAfter={t("yieldPolicy.unit")}
        style={{ width: 180 }}
        onChange={(value) => props.onChange({ ...props.draft, [field]: typeof value === "number" ? value : null })}
      />
      {error ? (
        <Typography.Text type="danger" role="alert" style={{ display: "block", fontSize: 12 }}>
          {t(`yieldPolicy.error.${error.reason}`, { field: label, minimum: error.minimum, maximum: error.maximum })}
        </Typography.Text>
      ) : (
        bound && (
          <Typography.Text type="secondary" style={{ display: "block", fontSize: 12 }}>
            {t("yieldPolicy.range", { minimum: bound.minimum, maximum: bound.maximum })}
          </Typography.Text>
        )
      )}
    </>
  );
}

// YieldPolicyFields shows the parameters that apply to the chosen policy and
// nothing else.
export default function YieldPolicyFields(props: FieldsProps) {
  const { t } = useTranslation("deployments");
  const { dictionary, draft, busy, modified, onChange } = props;
  const applies = applicable(dictionary, draft);
  const label = (field: string) => t(`yieldPolicy.field.${field}`);
  const number = (field: NumericField) => (
    <FieldRow key={field} label={label(field)} help={field === "unfinished_wait_sec" ? undefined : t(`yieldPolicy.help.${field}`)} modified={modified.has(field)}>
      <SecondsInput field={field} props={props} label={label(field)} />
    </FieldRow>
  );
  return (
    <>
      {applies.has("idle_sec") && number("idle_sec")}
      {applies.has("max_serve_sec") && number("max_serve_sec")}
      {applies.has("unfinished") && (
        <FieldRow label={label("unfinished")} modified={modified.has("unfinished")}>
          <div role="radiogroup" aria-label={label("unfinished")}>
            <Radio.Group
              value={draft.unfinished}
              disabled={busy}
              onChange={(event) => onChange(chooseUnfinished(dictionary, draft, event.target.value as YieldUnfinished))}
            >
              <Space direction="vertical" size={4}>
                {dictionary.unfinished.map((mode) => (
                  <Radio key={mode} value={mode}>{t(`yieldPolicy.unfinished.${mode}`)}</Radio>
                ))}
              </Space>
            </Radio.Group>
          </div>
          {applies.has("unfinished_wait_sec") && (
            <div style={{ marginTop: 8 }}>
              <Typography.Text style={{ display: "block", marginBottom: 4 }}>
                {label("unfinished_wait_sec")}
                {modified.has("unfinished_wait_sec") && <Tag color="orange" style={{ marginLeft: 8 }}>{t("yieldPolicy.modified")}</Tag>}
              </Typography.Text>
              <SecondsInput field="unfinished_wait_sec" props={props} label={label("unfinished_wait_sec")} />
            </div>
          )}
        </FieldRow>
      )}
      {applies.has("sleep_level") && (
        <FieldRow label={label("sleep_level")} modified={modified.has("sleep_level")}>
          <Select<YieldSleepLevel>
            size="small"
            value={draft.sleep_level}
            disabled={busy}
            aria-label={label("sleep_level")}
            style={{ width: 360, maxWidth: "100%" }}
            onChange={(value) => onChange({ ...draft, sleep_level: value })}
            options={dictionary.sleep_levels.map((value) => ({ value, label: t(`yieldPolicy.sleepLevel.${value}`) }))}
          />
        </FieldRow>
      )}
      {applies.has("hold_max_sec") && number("hold_max_sec")}
    </>
  );
}

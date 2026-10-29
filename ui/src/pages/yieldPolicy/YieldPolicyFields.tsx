import type { ReactNode } from "react";
import { Alert, InputNumber, Radio, Select, Space, Switch, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { YieldSleepLevel, YieldUnfinished } from "../../api";
import {
  type Applicable,
  type NumericField,
  type YieldDraft,
  type YieldFieldError,
  numericRanges,
  sleepLevels,
  unfinishedModes,
} from "./model";

// Props the parameter controls share: the working copy, which parameters
// apply, the refusals the server would give, whether the form is locked (a save
// is running) and the change handler.
export interface FieldsProps {
  draft: YieldDraft;
  use: Applicable;
  errors: YieldFieldError[];
  busy: boolean;
  // Whether the deployment may sleep by itself; one that cannot can only turn
  // the switch off.
  canAutoSleep: boolean;
  onChange: (patch: Partial<YieldDraft>) => void;
}

const rowStyle = { marginTop: 12 } as const;

// FieldRow is one labelled parameter: its name, the control, and a line of
// help below.
function FieldRow({ label, help, children }: { label: string; help?: ReactNode; children: ReactNode }) {
  return (
    <div role="group" aria-label={label} style={rowStyle}>
      <Typography.Text strong style={{ display: "block", marginBottom: 4 }}>{label}</Typography.Text>
      {children}
      {help && <Typography.Paragraph type="secondary" style={{ margin: "4px 0 0", fontSize: 12 }}>{help}</Typography.Paragraph>}
    </div>
  );
}

// SecondsInput is a number of seconds held to the server's range for the
// field; a value outside it is flagged, not clamped, so the operator sees what
// they typed.
function SecondsInput({ field, props, label }: { field: NumericField; props: FieldsProps; label: string }) {
  const { t } = useTranslation("deployments");
  const { minimum, maximum } = numericRanges[field];
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
        onChange={(value) => props.onChange({ [field]: typeof value === "number" ? value : null })}
      />
      {error ? (
        <Typography.Text type="danger" role="alert" style={{ display: "block", fontSize: 12 }}>
          {t(`yieldPolicy.error.${error.reason}`, { field: label, minimum, maximum })}
        </Typography.Text>
      ) : (
        <Typography.Text type="secondary" style={{ display: "block", fontSize: 12 }}>
          {t("yieldPolicy.range", { minimum, maximum })}
        </Typography.Text>
      )}
    </>
  );
}

// YieldPolicyFields shows the parameters that apply to the chosen policy and
// nothing else. The idle time is always listed: where it does not apply (never
// yield, or yield anytime, without automatic sleep) it reads as a dash.
export default function YieldPolicyFields(props: FieldsProps) {
  const { t } = useTranslation("deployments");
  const { draft, use, busy, onChange } = props;
  const label = (field: string) => t(`yieldPolicy.field.${field}`);
  return (
    <>
      <FieldRow label={label("idle_sec")} help={use.idleSec ? t("yieldPolicy.help.idle_sec") : t("yieldPolicy.help.idle_sec_none")}>
        {use.idleSec ? <SecondsInput field="idle_sec" props={props} label={label("idle_sec")} /> : <Typography.Text>{t("yieldPolicy.none")}</Typography.Text>}
      </FieldRow>
      <FieldRow label={label("auto_sleep")} help={t("yieldPolicy.help.auto_sleep")}>
        <Switch
          size="small"
          checked={draft.auto_sleep}
          disabled={busy || (!props.canAutoSleep && !draft.auto_sleep)}
          aria-label={label("auto_sleep")}
          onChange={(checked) => onChange({ auto_sleep: checked })}
        />
      </FieldRow>
      {use.sleepLevel && (
        <FieldRow label={label("sleep_level")}>
          <Select<YieldSleepLevel>
            size="small"
            value={draft.sleep_level}
            disabled={busy}
            aria-label={label("sleep_level")}
            style={{ width: 360, maxWidth: "100%" }}
            onChange={(value) => onChange({ sleep_level: value })}
            options={sleepLevels.map((value) => ({ value, label: t(`yieldPolicy.sleepLevel.${value}`) }))}
          />
        </FieldRow>
      )}
      {use.minServeSec && (
        <FieldRow label={label("min_serve_sec")} help={t("yieldPolicy.help.min_serve_sec")}>
          <SecondsInput field="min_serve_sec" props={props} label={label("min_serve_sec")} />
        </FieldRow>
      )}
      {use.unfinished && (
        <FieldRow label={label("unfinished")}>
          <div role="radiogroup" aria-label={label("unfinished")}>
            <Radio.Group
              value={draft.unfinished}
              disabled={busy}
              onChange={(event) => onChange({ unfinished: event.target.value as YieldUnfinished })}
            >
              <Space direction="vertical" size={4}>
                {unfinishedModes.map((mode) => (
                  <Radio key={mode} value={mode}>{t(`yieldPolicy.unfinished.${mode}`)}</Radio>
                ))}
              </Space>
            </Radio.Group>
          </div>
          {use.unfinishedWaitSec && (
            <div style={{ marginTop: 8 }}>
              <Typography.Text style={{ display: "block", marginBottom: 4 }}>{label("unfinished_wait_sec")}</Typography.Text>
              <SecondsInput field="unfinished_wait_sec" props={props} label={label("unfinished_wait_sec")} />
            </div>
          )}
          {draft.unfinished === "wait_unbounded" && (
            <Alert style={{ marginTop: 8 }} type="warning" showIcon message={t("yieldPolicy.unfinished.waitUnboundedWarning")} />
          )}
        </FieldRow>
      )}
      {use.holdMaxSec && (
        <FieldRow label={label("hold_max_sec")} help={t("yieldPolicy.help.hold_max_sec")}>
          <SecondsInput field="hold_max_sec" props={props} label={label("hold_max_sec")} />
        </FieldRow>
      )}
    </>
  );
}

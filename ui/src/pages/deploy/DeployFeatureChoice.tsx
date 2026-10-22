import { useEffect, useState } from "react";
import { AutoComplete, Form, Select, Switch } from "antd";
import type { EngineParamSpec } from "../../api";
import { localizedText } from "../../i18n/localizedText";
import DeployLogLevelField from "./DeployLogLevelField";
import DeployMetricsField from "./DeployMetricsField";
import { useEngineDecides } from "./deployParamApplicability";
import { useTranslation } from "react-i18next";

export default function FeatureChoice({
  spec,
  disabled,
}: {
  spec: EngineParamSpec;
  disabled?: boolean;
}) {
  const { t, i18n } = useTranslation("deploy");
  const form = Form.useFormInstance();
  const value = Form.useWatch(spec.key, { form, preserve: true });
  const boolean = spec.type === "boolean" || spec.control === "switch";
  // A boolean the dictionary gives no default, or whose default this launch
  // leaves to the engine, is the engine's to decide, so the card carries a
  // third position: auto leaves the key off the wire and the engine keeps —
  // and re-evaluates — its own default.
  const engineDecides = useEngineDecides(spec) && boolean;
  const defaultEnabled = boolean && spec.default === true && !engineDecides;
  const active = boolean
    ? value === undefined
      ? defaultEnabled
      : Boolean(value)
    : value !== undefined && value !== null && value !== "";
  const [editing, setEditing] = useState(active);
  const label = localizedText(
    spec.label,
    i18n.resolvedLanguage || i18n.language,
  );
  useEffect(() => {
    setEditing(active);
  }, [active, spec.key]);
  if (spec.key === "enable_metrics")
    return (
      <div className={`deploy-feature-card${active ? " is-active" : ""}`}>
        <DeployMetricsField spec={spec} compact />
      </div>
    );
  if (spec.key === "log_level")
    return <DeployLogLevelField spec={spec} compact disabled={disabled} />;
  if (boolean) {
    // Every boolean feature card carries the same control: a small select.
    // The closed select shows the position's short name; the open dropdown
    // adds the flag that position emits when this engine has a spelling for
    // that polarity — an engine whose only form is the negative one publishes
    // it as the key's flag, and that flag belongs to "off", never "on".
    const position = engineDecides
      ? value === undefined
        ? "auto"
        : value
          ? "on"
          : "off"
      : active
        ? "on"
        : "off";
    const flagHints: Record<string, string> = {
      auto: engineDecides ? t("workbench.engineDefaultHint") : "",
      on: spec.flag && spec.flag !== spec.flag_off ? spec.flag : "",
      off: spec.flag_off || "",
    };
    const description = engineDecides
      ? value === undefined
        ? t("workbench.engineDefault")
        : value
          ? t("workbench.enabled")
          : t("workbench.disabled")
      : active
        ? t("workbench.enabled")
        : t("workbench.disabled");
    // Auto is not lit: only a position the deployer chose marks the card.
    const lit = engineDecides ? value !== undefined : active;
    return (
      <div
        className={`deploy-feature-card${lit ? " is-active" : ""}`}
        title={engineDecides ? t("workbench.engineDefaultHint") : undefined}
      >
        <div className="deploy-feature-heading">
          <strong>{label}</strong>
          <Select
            size="small"
            disabled={disabled}
            aria-label={label}
            style={{ minWidth: 104 }}
            value={position}
            options={(engineDecides
              ? ["auto", "on", "off"]
              : ["on", "off"]
            ).map((p) => ({
              value: p,
              label:
                p === "auto"
                  ? t("workbench.triStateAuto")
                  : p === "on"
                    ? t("workbench.triStateOn")
                    : t("workbench.triStateOff"),
            }))}
            optionRender={(option) => (
              <span className="deploy-select-option">
                {String(option.label)}
                {flagHints[String(option.value)] ? (
                  <small>{flagHints[String(option.value)]}</small>
                ) : null}
              </span>
            )}
            onChange={(next) =>
              form.setFieldValue(
                spec.key,
                engineDecides
                  ? next === "auto"
                    ? undefined
                    : next === "on"
                  : next === "on",
              )
            }
          />
        </div>
        <small>{description}</small>
      </div>
    );
  }
  const options = (
    spec.options ||
    (spec.enum || []).map((option) => ({ value: option, label: option }))
  ).map((option) => ({
    value: option.value,
    label: localizedText(option.label, i18n.resolvedLanguage || i18n.language),
  }));
  const enabled = active || editing;
  const control =
    spec.control === "combobox" ? (
      <AutoComplete
        allowClear
        disabled={disabled || !enabled}
        aria-label={t("workbench.currentValue", { label })}
        value={value}
        placeholder={t("workbench.selectValue")}
        options={options}
        onChange={(next) => form.setFieldValue(spec.key, next)}
      />
    ) : (
      <Select
        allowClear
        disabled={disabled || !enabled}
        aria-label={t("workbench.currentValue", { label })}
        value={value}
        placeholder={t("workbench.selectValue")}
        options={options}
        onChange={(next) => form.setFieldValue(spec.key, next)}
      />
    );
  return (
    <div className={`deploy-feature-card${enabled ? " is-active" : ""}`}>
      <div className="deploy-feature-heading">
        <strong>{label}</strong>
        <Switch
          checked={enabled}
          disabled={disabled}
          aria-label={label}
          onChange={(checked) => {
            setEditing(checked);
            if (!checked)
              form.setFieldValue(
                spec.key,
                spec.control === "combobox" ? "" : undefined,
              );
            else if (spec.control !== "combobox")
              form.setFieldValue(spec.key, spec.default ?? options[0]?.value);
          }}
        />
      </div>
      {control}
    </div>
  );
}

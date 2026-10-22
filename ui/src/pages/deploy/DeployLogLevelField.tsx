import { Form, Select } from "antd";
import { useTranslation } from "react-i18next";
import type { EngineParamSpec } from "../../api";
import { localizedText } from "../../i18n/localizedText";

export default function DeployLogLevelField({
  spec,
  compact = false,
  disabled = false,
}: {
  spec: EngineParamSpec;
  compact?: boolean;
  disabled?: boolean;
}) {
  const { t, i18n } = useTranslation("deploy");
  const form = Form.useFormInstance();
  const value = Form.useWatch(spec.key, { form, preserve: true });
  const language = i18n.resolvedLanguage || i18n.language;
  const label = localizedText(spec.label, language);
  // Undefined is an untouched field. An explicit empty string means the user
  // chose no logging override, and survives re-renders and quick/expert swaps.
  const control = (
    <Select
      aria-label={label}
      disabled={disabled}
      size={compact ? "small" : undefined}
      style={{ minWidth: 120 }}
      value={value ?? spec.default ?? ""}
      options={[
        { value: "", label: t("workbench.unset") },
        ...(spec.options || []).map((option) => ({
          value: option.value,
          label: localizedText(option.label, language),
        })),
      ]}
      onChange={(next) => form.setFieldValue(spec.key, next)}
    />
  );
  const help =
    value === ""
      ? t("workbench.logLevelUnset")
      : localizedText(spec.help, language);
  return compact ? (
    <div className={`deploy-feature-card${value ? " is-active" : ""}`}>
      <div className="deploy-feature-heading">
        <strong>{label}</strong>
        {control}
      </div>
      <small>{help}</small>
    </div>
  ) : (
    <Form.Item label={label} extra={help}>
      {control}
    </Form.Item>
  );
}

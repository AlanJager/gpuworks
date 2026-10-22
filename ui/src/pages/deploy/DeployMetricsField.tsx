import { Alert, Form, Select, Switch } from "antd";
import { useTranslation } from "react-i18next";
import type { EngineParamSpec } from "../../api";
import { localizedText } from "../../i18n/localizedText";
import { useEngineDecides } from "./deployParamApplicability";

// The quick form keeps every feature in one row of equally sized cards, so the
// metrics switch renders as card copy there instead of a labelled form row
// whose help line and warning alert would make the card bigger than its
// neighbours. The expert dictionary keeps the labelled row.
export default function DeployMetricsField({
  spec,
  compact = false,
}: {
  spec: EngineParamSpec;
  compact?: boolean;
}) {
  const { t, i18n } = useTranslation("deploy");
  const form = Form.useFormInstance();
  const value = Form.useWatch(spec.key, { form, preserve: true });
  const enabled = Boolean(value);
  // Where this launch leaves the switch to the engine (its server or release
  // may refuse --enable-metrics), the control gains an "automatic" position
  // that sends nothing; on and off are still the deployer's to choose.
  const engineDecides = useEngineDecides(spec);
  // A hand-written command owns the launch configuration, so the dictionary
  // switch stops editing it and greys out (ADR 0020 decision 6).
  const manualCommand =
    Form.useWatch("command_mode", { form, preserve: true }) === "manual";
  const language = i18n.resolvedLanguage || i18n.language;
  const label = localizedText(spec.label, language);
  // Turning the switch off must store an explicit false: an undefined value
  // would fall back to the catalog arguments and keep --enable-metrics on.
  const setEnabled = (checked: boolean) =>
    form.setFieldValue(spec.key, checked);
  const control = engineDecides ? (
    <Select
      size={compact ? "small" : undefined}
      disabled={manualCommand}
      aria-label={label}
      style={{ minWidth: 104 }}
      value={value === undefined ? "auto" : value ? "on" : "off"}
      options={[
        { value: "auto", label: t("workbench.triStateAuto") },
        { value: "on", label: t("workbench.triStateOn") },
        { value: "off", label: t("workbench.triStateOff") },
      ]}
      onChange={(next) =>
        form.setFieldValue(
          spec.key,
          next === "auto" ? undefined : next === "on",
        )
      }
    />
  ) : (
    <Switch
      checked={enabled}
      disabled={manualCommand}
      aria-label={label}
      onChange={setEnabled}
    />
  );
  if (compact) {
    return (
      <>
        <div className="deploy-feature-heading">
          <strong>{label}</strong>
          {control}
        </div>
        <small>
          {engineDecides && value === undefined
            ? t("workbench.engineDefault")
            : t("workbench.metricsHint")}
        </small>
      </>
    );
  }
  if (engineDecides) {
    return (
      <div>
        <Form.Item label={label} extra={localizedText(spec.help, language)}>
          {control}
        </Form.Item>
        {!enabled && (
          <Alert
            type="warning"
            showIcon
            message={t("metrics.disabledWarning")}
          />
        )}
      </div>
    );
  }
  return (
    <div>
      <Form.Item
        label={label}
        name={spec.key}
        valuePropName="checked"
        extra={localizedText(spec.help, language)}
      >
        <Switch aria-label={label} disabled={manualCommand} />
      </Form.Item>
      {!enabled && (
        <Alert type="warning" showIcon message={t("metrics.disabledWarning")} />
      )}
    </div>
  );
}

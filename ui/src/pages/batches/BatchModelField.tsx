import { Alert, Select, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { stateLabel } from "../state";
import type { BatchModelOption } from "./batchModels";

/** The state of a model option in the reader's words. */
export function useModelStateText() {
  const { t } = useTranslation("batches");
  const { t: tCommon } = useTranslation("common");
  return (state: string): string => {
    if (state === "running") return t("create.model.state.serving");
    if (state === "asleep") return t("create.model.state.asleep");
    if (state === "asleepHeld") return t("create.model.state.asleepHeld");
    return stateLabel(state, (key) => tCommon(key)).text;
  };
}

/**
 * BatchModelField picks the model every request of the batch is sent to, or
 * none to use each line's own. The deployments that are active are offered,
 * asleep ones with their state; one a request does not wake is warned about.
 */
export default function BatchModelField({ options, value, onChange, disabled }: {
  options: BatchModelOption[];
  value: string | undefined;
  onChange: (model: string | undefined) => void;
  disabled: boolean;
}) {
  const { t } = useTranslation("batches");
  const stateText = useModelStateText();
  const chosen = options.find((option) => option.name === value);
  return (
    <div className="batch-create-field">
      <label htmlFor="batch-create-model">{t("create.model.label")}</label>
      <Select
        id="batch-create-model"
        allowClear
        value={value}
        onChange={(next) => onChange(next || undefined)}
        placeholder={t("create.model.placeholder")}
        disabled={disabled}
        options={options.map((option) => ({ value: option.name, label: `${option.name} · ${stateText(option.state)}` }))}
        style={{ width: "100%" }}
      />
      <Typography.Text type="secondary">{t("create.model.help")}</Typography.Text>
      {chosen?.state === "asleepHeld" && <Alert type="warning" showIcon message={t("create.model.heldWarning", { model: chosen.name })} />}
    </div>
  );
}

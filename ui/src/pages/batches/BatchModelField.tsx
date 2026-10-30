import { Alert, Select, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { BatchModel } from "../../batchApi";

/** The words of a model's state; the state is the server's (the batch dictionary). */
export function useModelStateText() {
  const { t } = useTranslation("batches");
  return (state: string): string => {
    if (state === "served") return t("create.model.state.serving");
    if (state === "asleep") return t("create.model.state.asleep");
    if (state === "asleep_held") return t("create.model.state.asleepHeld");
    return state;
  };
}

/**
 * BatchModelField picks the model every request of the batch is sent to, or
 * none to use each line's own. The server's list is offered, asleep ones with
 * their state; one a request does not wake is warned about.
 */
export default function BatchModelField({ options, value, onChange, disabled }: {
  options: BatchModel[];
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
      {chosen?.state === "asleep_held" && <Alert type="warning" showIcon message={t("create.model.heldWarning", { model: chosen.name })} />}
    </div>
  );
}

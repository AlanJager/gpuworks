import { useState } from "react";
import { Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type Model } from "../../api";

/**
 * The model's source id, editable in place when the server says it may be
 * changed (a local or URL model). Enter saves, Escape cancels.
 */
export default function ModelSourceIDField({
  model,
  onChanged,
}: {
  model: Model;
  onChanged: (model: Model) => void;
}) {
  const { t } = useTranslation("models");
  const [saving, setSaving] = useState(false);
  const save = async (value: string) => {
    const next = value.trim();
    if (!next || next === model.source_id || saving) return;
    setSaving(true);
    try {
      const updated = await api.updateModelSourceID(model.id, next);
      message.success(t("detail.message.sourceIDUpdated"));
      onChanged(updated);
    } catch (e) {
      message.error(t("detail.message.sourceIDFailed", { detail: (e as Error).message }));
    } finally {
      setSaving(false);
    }
  };
  if (!model.source_id_editable) return <>{model.source_id}</>;
  return (
    <>
      <Typography.Text
        editable={{
          onChange: (value) => void save(value),
          // The tooltip is also the edit button's accessible name.
          tooltip: t("detail.editSourceID"),
          text: model.source_id,
          triggerType: ["icon"],
          maxLength: 255,
        }}
      >
        {model.source_id}
      </Typography.Text>
      <Typography.Text type="secondary" style={{ display: "block", fontSize: 12 }}>
        {t("detail.editSourceIDHint")}
      </Typography.Text>
    </>
  );
}

import { Form, Input } from "antd";
import { useTranslation } from "react-i18next";
import type { Model } from "../../api";

export default function DeployServedNameField({
  model,
  hideLabel = false,
}: {
  model?: Model;
  hideLabel?: boolean;
}) {
  const { t } = useTranslation("deploy");
  const form = Form.useFormInstance();
  const value = Form.useWatch("served_name", { form, preserve: true });
  const fallback = model?.name || model?.source_id;
  return (
    <Form.Item
      label={hideLabel ? undefined : t("basic.servedName")}
      name="served_name"
      extra={
        !String(value || "").trim() && fallback
          ? t("basic.servedNameFallback", { name: fallback })
          : undefined
      }
    >
      <Input
        allowClear
        placeholder={fallback || "qwen3-8b"}
        aria-label={t("basic.servedName")}
      />
    </Form.Item>
  );
}

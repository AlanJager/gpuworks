import { useState } from "react";
import { Form, Input, Modal, Select, Switch, message } from "antd";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { api, type AIConfig, type AIPreset } from "../../api";

/** PRESETS are the endpoints the platform knows a base address and model for. */
export const PRESETS: { value: AIPreset; base_url: string; model: string }[] = [
  { value: "deepseek", base_url: "https://api.deepseek.com", model: "deepseek-v4-flash" },
  {
    value: "bailian",
    base_url: "https://dashscope.aliyuncs.com/compatible-mode/v1",
    model: "qwen-plus",
  },
  { value: "siliconflow", base_url: "https://api.siliconflow.cn/v1", model: "" },
  { value: "openrouter", base_url: "https://openrouter.ai/api/v1", model: "" },
  { value: "poe", base_url: "https://api.poe.com/v1", model: "GPT-5" },
  { value: "custom", base_url: "", model: "" },
];

export const presetLabel = (t: TFunction, preset: string) =>
  PRESETS.some((item) => item.value === preset) ? t(`ai.presetName.${preset}`) : preset;

interface FormValues {
  preset: AIPreset;
  name: string;
  base_url: string;
  model: string;
  api_key?: string;
  is_default: boolean;
}

/**
 * AIConfigModal adds an AI service, or edits one in place. An edit is
 * prefilled from the stored service and leaves the key blank, because the API
 * never sends a key back: a blank field keeps the stored one, and typing a new
 * key replaces it.
 */
export default function AIConfigModal({
  open,
  editing,
  onClose,
  onDone,
}: {
  open: boolean;
  editing?: AIConfig;
  onClose: () => void;
  onDone: () => void;
}) {
  const { t } = useTranslation("settings");
  const [form] = Form.useForm<FormValues>();
  const [submitting, setSubmitting] = useState(false);

  const onPreset = (value: AIPreset) => {
    const p = PRESETS.find((x) => x.value === value);
    if (p) form.setFieldsValue({ base_url: p.base_url, model: p.model });
  };

  const initialValues: FormValues = editing
    ? {
        preset: editing.preset as AIPreset,
        name: editing.name,
        base_url: editing.base_url,
        model: editing.model,
        api_key: "",
        is_default: editing.is_default,
      }
    : {
        preset: "deepseek",
        name: "",
        base_url: "https://api.deepseek.com",
        model: "deepseek-v4-flash",
        api_key: "",
        is_default: true,
      };

  const submit = async () => {
    // A rejected validation leaves the dialog open on the fields it marked;
    // it is not a failed save, so it shows no message.
    const v = await form.validateFields().catch(() => null);
    if (!v) return;
    setSubmitting(true);
    try {
      if (editing) {
        await api.updateAIConfig(editing.id, {
          name: v.name,
          preset: v.preset,
          base_url: v.base_url,
          model: v.model,
          is_default: !!v.is_default,
          // Left out unless typed, so the stored key survives the edit.
          ...(v.api_key ? { api_key: v.api_key } : {}),
        });
        message.success(t("ai.updated"));
      } else {
        await api.createAIConfig({
          name: v.name,
          preset: v.preset,
          base_url: v.base_url,
          api_key: v.api_key ?? "",
          model: v.model,
          is_default: !!v.is_default,
        });
        message.success(t("ai.added"));
      }
      form.resetFields();
      onDone();
    } catch (e) {
      message.error(t(editing ? "error.update" : "error.add", { detail: (e as Error).message }));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal
      title={editing ? t("ai.editService") : t("ai.addService")}
      open={open}
      onCancel={onClose}
      onOk={submit}
      confirmLoading={submitting}
      okText={editing ? t("common.save") : t("common.add")}
      cancelText={t("common.cancel")}
      destroyOnHidden
    >
      <Form
        key={editing ? `edit-${editing.id}` : "add"}
        form={form}
        layout="vertical"
        initialValues={initialValues}
      >
        <Form.Item label={t("ai.preset")} name="preset" rules={[{ required: true }]}>
          <Select options={PRESETS.map((preset) => ({ value: preset.value, label: presetLabel(t, preset.value) }))} onChange={onPreset} />
        </Form.Item>
        <Form.Item label={t("ai.name")} name="name" rules={[{ required: true, message: t("ai.nameRequired") }]}>
          <Input placeholder={t("ai.namePlaceholder")} />
        </Form.Item>
        <Form.Item
          label={t("ai.baseUrl")}
          name="base_url"
          rules={[{ required: true, message: t("ai.baseUrlRequired") }]}
        >
          <Input placeholder="https://api.deepseek.com" />
        </Form.Item>
        <Form.Item label={t("ai.model")} name="model" rules={[{ required: true, message: t("ai.modelRequired") }]}>
          <Input placeholder="deepseek-v4-flash" />
        </Form.Item>
        <Form.Item
          label={t("ai.upstreamKey")}
          name="api_key"
          rules={editing ? [] : [{ required: true, message: t("ai.keyRequired") }]}
        >
          <Input.Password placeholder={editing ? t("ai.keyKeep") : "sk-…"} autoComplete="new-password" />
        </Form.Item>
        <Form.Item label={t("ai.setDefault")} name="is_default" valuePropName="checked">
          <Switch />
        </Form.Item>
      </Form>
    </Modal>
  );
}

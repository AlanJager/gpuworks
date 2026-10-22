import { Input, InputNumber, Select, Space, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { InputField } from "../../inferenceApi";
import { inputLabel } from "./compose";

export default function InputsForm({ fields, values, onChange }: { fields: InputField[]; values: Record<string, unknown>; onChange: (name: string, value: unknown) => void }) {
  const { t } = useTranslation("playground");
  return <Space direction="vertical" style={{ width: "100%" }}>{fields.map((field) => {
    const label = inputLabel(field);
    const value = values[field.name];
    let control;
    if (field.kind === "number") {
      control = <InputNumber aria-label={label} value={value as number | undefined} onChange={(next) => onChange(field.name, next)} style={{ width: "100%" }} />;
    } else if (field.kind === "select" && field.options?.length) {
      control = <Select aria-label={label} value={value as string | undefined} placeholder={t("inputs.select")} options={field.options.map((option) => ({ value: option, label: option }))} onChange={(next) => onChange(field.name, next)} style={{ width: "100%" }} />;
    } else if (field.kind === "json") {
      control = <Input.TextArea aria-label={label} value={typeof value === "string" ? value : value ? JSON.stringify(value) : ""} onChange={(event) => onChange(field.name, event.target.value)} placeholder="{}" />;
    } else if (field.kind === "audio" || field.kind === "image" || field.kind === "file") {
      control = <input aria-label={label} type="file" accept={field.kind === "audio" ? "audio/*" : field.kind === "image" ? "image/*" : undefined} onChange={(event) => { const file = event.target.files?.[0]; if (!file) return; const reader = new FileReader(); reader.onload = () => onChange(field.name, String(reader.result)); reader.readAsDataURL(file); }} />;
    } else {
      control = <Input aria-label={label} value={value as string | undefined} placeholder={field.kind === "select" ? t("inputs.discoveryPending") : undefined} onChange={(event) => onChange(field.name, event.target.value)} />;
    }
    return <label key={field.name} className="playground-declared-input"><Typography.Text strong>{label}{field.required ? " *" : ""}</Typography.Text>{control}</label>;
  })}</Space>;
}

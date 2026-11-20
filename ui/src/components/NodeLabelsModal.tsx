import { useEffect, useRef, useState } from "react";
import { Controller, useFieldArray, useForm } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { api, type Node } from "../api";
import { message } from "../ui-kit/feedback";
import { formSubmit } from "../ui-kit/form/submit";
import { ConsoleIcon } from "../ui-kit/consoleIcons";
import { Button } from "./ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "./ui/dialog";
import { Field, FieldError } from "./ui/field";
import { Input } from "./ui/input";

type LabelRow = { key: string; value: string };
type Values = { labels: LabelRow[] };
const DIALOG_STYLE = { width: "min(520px, calc(100vw - 32px))", maxWidth: "calc(100vw - 32px)" };

export default function NodeLabelsModal({ node, onClose, onSaved }: { node: Node | null; onClose: () => void; onSaved: (node: Node) => void }) {
  const { t } = useTranslation("nodes");
  const { t: tCommon } = useTranslation("common");
  const form = useForm<Values>({ shouldFocusError: false, defaultValues: { labels: [] } });
  const rows = useFieldArray({ control: form.control, name: "labels" });
  const formRef = useRef<HTMLFormElement>(null);
  const [saving, setSaving] = useState(false);
  useEffect(() => {
    form.reset({ labels: node ? Object.entries(node.labels ?? {}).map(([key, value]) => ({ key, value })) : [] });
  }, [form, node]);
  async function save({ labels }: Values) {
    if (!node) return;
    const keys = labels.map((row) => row.key);
    if (new Set(keys).size !== keys.length) { message.error(t("labels.duplicate")); return; }
    setSaving(true);
    try {
      const updated = await api.updateNodeLabels(node.uuid, Object.fromEntries(labels.map((row) => [row.key, row.value ?? ""])));
      message.success(t("labels.saved")); onSaved(updated);
    } catch (error) { message.error(t("labels.failed", { detail: (error as Error).message })); }
    finally { setSaving(false); }
  }
  return <Dialog open={!!node} onOpenChange={(open) => { if (!open) onClose(); }}>
    <DialogContent closeButtonLabel={tCommon("actions.close")} style={DIALOG_STYLE}>
      <DialogHeader>
        <DialogTitle>{t("labels.title")}</DialogTitle>
        <DialogDescription>{t("labels.help")}</DialogDescription>
      </DialogHeader>
      <form ref={formRef} noValidate onSubmit={formSubmit(form, () => formRef.current, save)} className="flex flex-col gap-3">
        {rows.fields.map((row, index) => <div key={row.id} className="flex items-start gap-2">
          <Controller control={form.control} name={`labels.${index}.key`}
            rules={{ validate: (value) => value.trim() !== "" || t("labels.keyRequired") }}
            render={({ field, fieldState }) => <Field name={field.name} invalid={fieldState.invalid} data-form-field={field.name} className="flex-1">
              <Input {...field} aria-label={t("labels.key")} aria-invalid={fieldState.invalid} placeholder={t("labels.key")} maxLength={128} />
              {fieldState.error?.message && <FieldError match role="alert">{fieldState.error.message}</FieldError>}
            </Field>} />
          <Controller control={form.control} name={`labels.${index}.value`}
            render={({ field }) => <Input {...field} className="flex-1" aria-label={t("labels.value")} placeholder={t("labels.value")} maxLength={256} />} />
          <Button type="button" variant="ghost" iconOnly aria-label={t("labels.remove")} onClick={() => rows.remove(index)}><ConsoleIcon.delete size={16} /></Button>
        </div>)}
        <div>
          <Button type="button" variant="tertiary" dashed leadingIcon={ConsoleIcon.add} onClick={() => rows.append({ key: "", value: "" })}>{t("labels.add")}</Button>
        </div>
        <DialogFooter>
          <Button type="button" variant="ghost" onClick={onClose}>{t("action.cancel")}</Button>
          <Button type="submit" variant="primary" loading={saving}>{t("action.save")}</Button>
        </DialogFooter>
      </form>
    </DialogContent>
  </Dialog>;
}

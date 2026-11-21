import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { APIError, apiErrorText } from "../../apiError";
import type { SlurmCluster, SlurmClusterEdit } from "../../slurmApi";
import { slurmDurationText, useSlurmDefaults } from "../../slurmClusters";
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from "../../components/ui/accordion";
import { Button } from "../../components/ui/button";
import { Input } from "../../components/ui/input";
import { FormField } from "../../ui-kit/form/FormField";
import { formSubmit } from "../../ui-kit/form/submit";
import { Notice } from "../../ui-kit/Notice";
import { NumberInput } from "../../ui-kit/NumberInput";
import SlurmApplyModal from "./SlurmApplyModal";
import SlurmSetupCommand from "./SlurmSetupCommand";
import { ADVANCED_FIELDS, FIELD_OF, defaultsOf, formSchema, isQuickTunnelAddress, requestOf, updateOf, type FormValues, type SlurmClusterPrefill } from "./slurmClusterForm";

export type { SlurmClusterPrefill };

/**
 * Connects a Slurm cluster: the form, then the command to run on the
 * controller. The hook owns the form and hands back its body and its footer
 * buttons, so the same form sits in the Add External Platform dialog (a
 * cluster of its own) and in a drawer (an E-HPC cluster of a cloud account,
 * prefilled, or an entry being edited). `open` says the container is showing:
 * each opening starts from a fresh form. onCreated runs as soon as the entry
 * exists; the body then stays on the command until the container is closed.
 *
 * With a prefill the entry is made from a cloud account's cluster. Making the
 * entry and setting the cluster up are separate steps, as for a Kubernetes
 * entry: the submit button only creates it, and the result then offers
 * "Check and apply" when the server says the platform can set that cluster up
 * itself (`can_apply`; the check changes nothing and the operator confirms
 * before anything is applied), or says why it cannot, and always shows the
 * commands to run by hand. With `edit` it changes that entry instead: the
 * server address, the agents' account and the two directories are shown, fixed.
 */
export function useSlurmClusterForm({ open, onCreated, onEdited, onClose, prefill, edit }: {
  open: boolean;
  onCreated: () => void;
  /** Called when an edit was saved. */
  onEdited?: () => void;
  /** Closes the container: an edit that changed nothing closes it. */
  onClose?: () => void;
  prefill?: SlurmClusterPrefill;
  /** The entry to edit; absent connects a new one. */
  edit?: SlurmCluster;
}): {
  body: ReactNode;
  /** The buttons for the container's footer: Cancel and the submit buttons, then Close once there is nothing more to fill in. */
  footer: (onClose: () => void) => ReactNode;
  saving: boolean;
  /** The entry was made: the body is now the command to run, no longer the form. */
  created: boolean;
} {
  const { t } = useTranslation("slurm");
  const { t: tCommon } = useTranslation("common");
  const schema = useMemo(() => formSchema(t), [t]);
  const form = useForm<FormValues>({ resolver: zodResolver(schema), shouldFocusError: false, defaultValues: defaultsOf(prefill, edit) });
  const { defaults, error: defaultsError } = useSlurmDefaults(open);
  const formId = useId();
  const formRef = useRef<HTMLFormElement>(null);
  const nameInput = useRef<HTMLInputElement | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [created, setCreated] = useState<SlurmCluster | null>(null);
  const [applying, setApplying] = useState(false);
  const [edited, setEdited] = useState<SlurmClusterEdit | null>(null);
  const [advancedOpen, setAdvancedOpen] = useState(false);

  // Each opening starts from the defaults (and the prefill or the entry), like a fresh form. The list the entry comes
  // from is read again every few seconds, so what restarts the form is which entry or cluster it is for, not the
  // identity of the object.
  useEffect(() => {
    if (!open) return;
    form.reset(defaultsOf(prefill, edit));
    setCreated(null);
    setApplying(false);
    setEdited(null);
    setError(null);
    setAdvancedOpen(false);
    // The container takes focus itself when it opens; hand it to the first field once it has.
    const timer = setTimeout(() => nameInput.current?.focus(), 0);
    return () => clearTimeout(timer);
  }, [open, prefill?.provider_id, prefill?.provider_cluster_id, edit?.uuid]);

  const submit = async (values: FormValues) => {
    setSaving(true);
    setError(null);
    try {
      if (edit) {
        const update = updateOf(values, edit, defaults);
        if (Object.keys(update).length === 0) { onClose?.(); return; }
        const result = await api.updateSlurmCluster(edit.uuid, update);
        onEdited?.();
        setEdited(result);
      } else {
        const cluster = await api.createSlurmCluster(requestOf(values, prefill));
        setCreated(cluster);
        onCreated();
      }
    } catch (e) {
      const text = apiErrorText(tCommon, e);
      const field = e instanceof APIError
        ? e.code === "SLURM_CLUSTER_NAME_TAKEN" ? "name" : e.code === "SLURM_CLUSTER_INVALID" ? FIELD_OF[String(e.params?.field ?? "")] : undefined
        : undefined;
      if (field) {
        form.setError(field, { type: "server", message: text });
        if (ADVANCED_FIELDS.has(field)) setAdvancedOpen(true);
      } else setError(text);
    } finally {
      setSaving(false);
    }
  };

  const fixed = Boolean(edit);
  // The platform's own temporary address (the server says which): said once on the field, never refused.
  const quick = !fixed && isQuickTunnelAddress(form.watch("server_url"), defaults?.quick_tunnel_url);
  const range = (min?: number, max?: number) => defaults && min !== undefined && max !== undefined ? { min, max } : undefined;
  const idleRange = range(defaults?.idle_return_min_seconds, defaults?.idle_return_max_seconds);
  const pendingRange = range(defaults?.pending_timeout_min_seconds, defaults?.pending_timeout_max_seconds);
  const advanced = (
    <div className="flex flex-col gap-4 pt-2">
      <FormField control={form.control} name="run_as" label={t("form.runAs")} description={fixed ? t("form.fixedAfterCreate") : t("form.runAsHelp")}>
        {(binding) => <Input {...binding} placeholder={defaults?.run_as} disabled={fixed} />}
      </FormField>
      <FormField control={form.control} name="account_user" label={t("form.accountUser")} description={t("form.accountUserHelp")}>
        {(binding) => <Input {...binding} placeholder={t("form.accountUserPlaceholder")} />}
      </FormField>
      <FormField control={form.control} name="install_dir" label={t("form.installDir")} description={fixed ? t("form.fixedAfterCreate") : t("form.installDirHelp")}>
        {(binding) => <Input {...binding} placeholder={defaults?.install_dir} disabled={fixed} />}
      </FormField>
      <FormField control={form.control} name="script_dir" label={t("form.scriptDir")} description={fixed ? t("form.fixedAfterCreate") : t("form.scriptDirHelp")}>
        {(binding) => <Input {...binding} placeholder={defaults?.script_dir} disabled={fixed} />}
      </FormField>
      <FormField control={form.control} name="sbatch_args" label={t("form.sbatchArgs")} description={t("form.sbatchArgsHelp")}>
        {(binding) => <Input {...binding} placeholder={t("form.sbatchArgsPlaceholder")} />}
      </FormField>
      <FormField control={form.control} name="idle_return_seconds" label={t("form.idleReturn")} description={idleRange ? t("form.idleReturnHelpRange", idleRange) : t("form.idleReturnHelp")}>
        {(binding) => (
          <div className="flex items-center gap-2">
            <NumberInput {...binding} min={defaults?.idle_return_min_seconds} max={defaults?.idle_return_max_seconds} precision={0} placeholder={defaults ? String(defaults.idle_return_seconds) : undefined} className="w-32" />
            <span className="text-fg-muted">{t("form.secondsUnit")}</span>
          </div>
        )}
      </FormField>
      <FormField control={form.control} name="pending_timeout_seconds" label={t("form.pendingTimeout")} description={pendingRange ? t("form.pendingTimeoutHelpRange", pendingRange) : t("form.pendingTimeoutHelp")}>
        {(binding) => (
          <div className="flex items-center gap-2">
            <NumberInput {...binding} min={defaults?.pending_timeout_min_seconds} max={defaults?.pending_timeout_max_seconds} precision={0} placeholder={defaults ? String(defaults.pending_timeout_seconds) : undefined} className="w-32" />
            <span className="text-fg-muted">{t("form.secondsUnit")}</span>
          </div>
        )}
      </FormField>
    </div>
  );

  const body = edited ? (
    <div className="flex flex-col gap-4">
      <Notice tone="info" title={t("edit.savedTitle", { name: edited.cluster.name })}>
        {defaults ? t("edit.savedHelp", { interval: slurmDurationText(t, defaults.report_interval_seconds) }) : t("edit.savedHelpUnknown")}
      </Notice>
      <div className="flex flex-col gap-1">
        <strong className="font-semibold text-fg-default">{t("edit.changed")}</strong>
        <ul className="m-0 list-disc ps-5">
          {edited.changed.map((name) => <li key={name}>{t(`edit.field.${name}`, { defaultValue: name })}</li>)}
        </ul>
      </div>
    </div>
  ) : created ? (
    <div className="flex flex-col gap-4">
      <Notice tone="info" title={t("created.title", { name: created.name })}>{created.source ? t("created.noteSource") : t("created.note")}</Notice>
      {created.source && created.can_apply && (
        <div className="flex flex-col items-start gap-2">
          <strong className="font-semibold text-fg-default">{t("created.byGpuworks")}</strong>
          <span className="text-fg-muted">{t("created.byGpuworksNote")}</span>
          <Button variant="primary" onClick={() => setApplying(true)}>{t("created.applyNow")}</Button>
          <strong className="mt-2 font-semibold text-fg-default">{t("created.byHand")}</strong>
        </div>
      )}
      {created.source && !created.can_apply && created.operation_blocked && (
        <Notice tone="warning" title={t("created.applyBlocked")}>{t(`blocked.${created.operation_blocked}`)}</Notice>
      )}
      {created.setup_command && <SlurmSetupCommand command={created.setup_command} checkCommand={created.check_command} />}
      {applying && <SlurmApplyModal cluster={created} onClose={() => setApplying(false)} onDone={onCreated} />}
    </div>
  ) : (
    <form id={formId} ref={formRef} noValidate onSubmit={formSubmit(form, () => formRef.current, (values) => submit(values))} className="flex flex-col gap-4">
      {/* The footer's buttons are outside the form; this one lets Enter in a field submit it. */}
      <button type="submit" hidden aria-hidden="true" tabIndex={-1} />
      {error && <Notice tone="danger" title={error} />}
      {defaultsError && <Notice tone="warning" title={t("form.defaultsFailed")} children={t("form.defaultsFailedHelp")} />}
      {prefill && <Notice tone="info" title={t("form.fromSource", { source: prefill.source_label })} />}
      <FormField control={form.control} name="name" label={t("form.name")} required>
        {({ ref, ...binding }) => <Input {...binding} ref={(node) => { ref(node); nameInput.current = node; }} placeholder={t("form.namePlaceholder")} />}
      </FormField>
      <FormField control={form.control} name="server_url" label={t("form.serverUrl")} description={fixed ? t("form.fixedAfterCreate") : quick ? <span className="text-fg-warning">{t("form.serverUrlQuick")}</span> : t("form.serverUrlHelp")} required>
        {(binding) => <Input {...binding} disabled={fixed} />}
      </FormField>
      <Accordion type="single" collapsible value={advancedOpen ? "advanced" : ""} onValueChange={(next: string) => setAdvancedOpen(next === "advanced")}>
        <AccordionItem value="advanced">
          <AccordionTrigger>{t("form.advanced")}</AccordionTrigger>
          <AccordionContent>{advanced}</AccordionContent>
        </AccordionItem>
      </Accordion>
    </form>
  );

  const footer = (close: () => void) => created || edited ? (
    <Button variant="primary" onClick={close}>{t("form.close")}</Button>
  ) : (
    <>
      <Button variant="ghost" onClick={close} disabled={saving}>{t("form.cancel")}</Button>
      <Button type="submit" form={formId} variant="primary" loading={saving}>{edit ? t("form.save") : t("form.submit")}</Button>
    </>
  );
  return { body, footer, saving, created: created !== null };
}

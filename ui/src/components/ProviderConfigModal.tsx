import { useEffect, useId, useRef, useState } from "react";
import { useForm, type Control } from "react-hook-form";
import { useTranslation } from "react-i18next";
import {
  api,
  type CloudProvider,
  type KindResource,
  type ProviderCategoryInfo,
  type ProviderCredentialField,
  type ProviderKindInfo,
  type ProviderTestOverride,
} from "../api";
import { localizedText } from "../i18n/localizedText";
import { clusterFormKind, clusterCategoryOf } from "../platformClusters";
import { useKubeClusterForm } from "../pages/kubernetes/useKubeClusterForm";
import { useSlurmClusterForm } from "../pages/slurm/useSlurmClusterForm";
import { PlatformResourceBadge } from "../pages/platforms/PlatformResourceBadge";
import ProviderTestDialog, { NOOP_TEST_RUN, type ProviderTestRun } from "./ProviderTestDialog";
import { FormField } from "../ui-kit/form/FormField";
import { formSubmit } from "../ui-kit/form/submit";
import { Notice } from "../ui-kit/Notice";
import { PasswordInput } from "../ui-kit/PasswordInput";
import { Button } from "./ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "./ui/dialog";
import { Field, FieldLabel } from "./ui/field";
import { Input } from "./ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger } from "./ui/select";
import { TabItem, Tabs, TabsList } from "./ui/tabs";

/** What the vendors' form holds: the name, and the credential fields the chosen kind's schema declares. */
interface FormValues {
  name: string;
  endpoint: string;
  api_key: string;
  public_key: string;
  private_key: string;
  extra: Record<string, string>;
}
const BLANK: FormValues = { name: "", endpoint: "", api_key: "", public_key: "", private_key: "", extra: {} };
type CredentialPath = "endpoint" | "api_key" | "public_key" | "private_key" | `extra.${string}`;

/**
 * Adds an external platform, or edits a cloud account. The platform type is
 * chosen first (public cloud, private cloud, a Kubernetes cluster, a Slurm
 * cluster), then the vendor among that type's; a cloud vendor's account asks
 * for its credentials, and a cluster, which is no vendor's account, for its
 * cluster form, followed by the command to run in the cluster.
 */
export default function ProviderConfigModal({
  open,
  provider,
  defaultKind,
  kindInfos,
  categories = [],
  providerLabel,
  onClose,
  onSaved,
  canConnectCluster = false,
  onClusterCreated,
}: {
  open: boolean;
  provider?: CloudProvider | null;
  defaultKind?: string | null;
  kindInfos: ProviderKindInfo[];
  /** The platform types to offer, in order, each naming its kinds (GET /providers/kinds categories). */
  categories?: ProviderCategoryInfo[];
  providerLabel: string;
  onClose: () => void;
  /** The account was saved; a new one is named, so its row can be opened. */
  onSaved: (added?: { id: number; kind: string }) => void;
  /** Offers the clusters among the platforms: one connected on its own, with a command. Making its enrollment token takes an administrator. */
  canConnectCluster?: boolean;
  /** A cluster was connected from this dialog; it stays open on the command to run. */
  onClusterCreated?: () => void;
}) {
  const { t, i18n } = useTranslation("providers");
  const { t: tCommon } = useTranslation("common");
  const language = i18n.resolvedLanguage || i18n.language;
  const form = useForm<FormValues>({ shouldFocusError: false, defaultValues: BLANK });
  const formRef = useRef<HTMLFormElement>(null);
  const footerRef = useRef<HTMLDivElement>(null);
  const formId = useId();
  const kindId = useId();
  const connectsId = useId();
  const [kind, setKind] = useState<string>("");
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [testJob, setTestJob] = useState<{ run: ProviderTestRun } | null>(null);
  const [loadedProviderID, setLoadedProviderID] = useState<number | null>(null);
  const [loadError, setLoadError] = useState(false);
  const editing = !!provider;
  // A cluster is not a cloud vendor's account: its form is the cluster form, below the choice. Each is offered as the
  // platform type the server marks as added with a cluster form; its id is also the choice's.
  const kubeCategory = canConnectCluster ? clusterCategoryOf(categories, "kubernetes") : undefined;
  const slurmCategory = canConnectCluster ? clusterCategoryOf(categories, "slurm") : undefined;
  const kubernetes = !provider && !!kubeCategory && kind === kubeCategory.id;
  const slurm = !provider && !!slurmCategory && kind === slurmCategory.id;
  const clusterChosen = kubernetes || slurm;
  const kubeForm = useKubeClusterForm({ open: open && kubernetes, onCreated: () => onClusterCreated?.() });
  const slurmForm = useSlurmClusterForm({ open: open && slurm, onCreated: () => onClusterCreated?.() });
  const cluster = slurm ? slurmForm : kubeForm;
  const busy = submitting || cluster.saving;
  const close = () => {
    if (busy) return;
    form.reset(BLANK);
    setKind("");
    setSubmitError(null);
    setTestJob(null);
    onClose();
  };
  const editReady = !provider || (loadedProviderID === provider.id && kindInfos.some((info) => info.kind === provider.kind));
  const locked = !editReady || loadError || submitting;
  const kindLabel = (k: string) => t(`kind.${k}`, { defaultValue: localizedText(kindInfos.find((info) => info.kind === k)?.label, language) || k });
  const selectedKind = kindInfos.find((k) => k.kind === kind);
  const requirement = t(`config.requirements.${provider?.kind ?? kind}`, { defaultValue: "" });
  // What the chosen platform connects: the vendor's resources, or for a cluster the cluster itself.
  const connects: KindResource[] = kubernetes ? kubeCategory?.resources ?? [] : slurm ? slurmCategory?.resources ?? [] : selectedKind?.resources ?? [];
  // The platform types to choose among, in the server's order, and the one the chosen platform is of.
  const groups = categories
    .filter((category) => clusterFormKind(category) ? category === kubeCategory || category === slurmCategory : category.kinds.length > 0)
    .map((category) => ({ category: category.id, cluster: clusterFormKind(category), kinds: clusterFormKind(category) ? [category.id] : category.kinds }));
  const group = groups.find((g) => g.kinds.includes(kind)) ?? groups[0];
  const groupLabel = (g: { category: string; cluster: string | null }) => g.cluster === "kubernetes" ? t("config.kubernetesGroup") : g.cluster === "slurm" ? t("config.slurmGroup") : t(`category.${g.category}`, { defaultValue: t("config.otherCategory") });

  // The initial kind is applied once per open (and again when the provider or
  // the preselected kind changes), never in reaction to the user's own pick:
  // the seed key remembers what was applied, so a choice in the platform
  // select is not snapped back to the first kind.
  const seededRef = useRef("");
  useEffect(() => {
    if (!open) {
      seededRef.current = "";
      return;
    }
    const forCluster = !provider && [kubeCategory, slurmCategory].some((category) => !!category && defaultKind === category.id);
    // The cluster form needs nothing from the vendors' schema; every other choice waits for it.
    if (kindInfos.length === 0 && !forCluster) return;
    const seed = `${provider?.id ?? ""}|${provider?.kind ?? ""}|${defaultKind ?? ""}`;
    if (seededRef.current === seed) return;
    seededRef.current = seed;
    const next = provider?.kind
      ? provider.kind
      : forCluster || (defaultKind && kindInfos.some((k) => k.kind === defaultKind))
        ? defaultKind!
        : kindInfos[0].kind;
    setKind(next);
    setSubmitError(null);
    if (provider) {
      if (loadedProviderID === provider.id) return;
      form.reset({ ...BLANK, name: provider.name });
    } else {
      form.reset({ ...BLANK, ...credentialDefaults(kindInfos.find((info) => info.kind === next)) });
    }
  }, [open, provider, defaultKind, kindInfos, form, loadedProviderID, kubeCategory, slurmCategory]);

  // Fetch on every edit opening so refreshes and other sessions cannot leave
  // stale overrides in the form. A closed/replaced dialog ignores late replies.
  const providerID = provider?.id;
  useEffect(() => {
    setLoadedProviderID(null);
    setLoadError(false);
    if (!open || providerID === undefined) return;
    let cancelled = false;
    api.provider(providerID).then((config) => {
      if (cancelled) return;
      form.reset({ ...BLANK, name: config.name, endpoint: config.endpoint ?? "", public_key: config.public_key ?? "", extra: { ...config.extra } });
      setLoadedProviderID(providerID);
    }).catch(() => {
      if (!cancelled) setLoadError(true);
    });
    return () => { cancelled = true; };
  }, [open, providerID, form]);

  // Once the cluster exists its form is gone, and the focus it held with it: the button that closes the dialog takes it.
  useEffect(() => {
    if (cluster.created) footerRef.current?.querySelector("button")?.focus();
  }, [cluster.created]);

  const chooseKind = (next: string) => {
    setKind(next);
    setSubmitError(null);
    // The name typed so far stays; the credentials are the new kind's.
    form.reset({ ...BLANK, name: form.getValues("name"), ...credentialDefaults(kindInfos.find((info) => info.kind === next)) });
  };

  const submit = async (values: FormValues) => {
    // Nothing is added without a platform chosen.
    if (locked || (!provider && !selectedKind)) return;
    setSubmitting(true);
    setSubmitError(null);
    try {
      const body = requestOf(values, selectedKind?.credential_fields ?? []);
      let made: { id: number; kind: string } | undefined;
      if (provider) await api.updateProvider(provider.id, body);
      else made = await api.addProvider({ kind, ...body });
      form.reset(BLANK);
      setKind("");
      setTestJob(null);
      onSaved(made && { id: made.id, kind: made.kind });
    } catch (e) {
      setSubmitError(t("config.operationFailed", { operation: t(provider ? "config.operation.update" : "config.operation.add"), detail: (e as Error).message }));
    } finally {
      setSubmitting(false);
    }
  };

  const startTest = () => {
    // A test that is already open is the one this button would start.
    if (testJob) return;
    const values = testValuesOf(form.getValues());
    if (provider) {
      // A saved account is tested on the server over what it stores: the form shows only what may be read back
      // (never a secret), so what it holds overrides the stored values and a field left blank keeps its own.
      const override = Object.keys(values).length > 0 ? values : undefined;
      setTestJob({ run: (onCheck, signal) => api.testSavedProviderConnection(provider.id, onCheck, override, signal) });
      return;
    }
    setTestJob({ run: (onCheck, signal) => api.testProviderConnection({ kind, ...values }, onCheck, signal) });
  };

  return (
    <>
    <Dialog open={open} onOpenChange={(next) => { if (!next) close(); }} disablePointerDismissal>
      <DialogContent size="lg" closeButtonLabel={tCommon("actions.close")} closeButtonDisabled={busy} className="flex max-h-[calc(100dvh-32px)] flex-col gap-4">
        <DialogHeader>
          <DialogTitle>{t("config.title", { operation: t(provider ? "config.edit" : "config.add"), provider: providerLabel })}</DialogTitle>
        </DialogHeader>
        {/* The fields scroll; the title and the buttons stay. The inset keeps focus rings clear of the scroll edge. */}
        <div className="-mx-1 flex min-h-0 flex-col gap-4 overflow-auto px-1 pb-1">
          {loadError && <Notice tone="danger" title={t("config.loadFailed")} />}
          {/* Once the cluster exists the dialog shows only what to run next. */}
          {!(clusterChosen && cluster.created) && (
            <>
              {provider ? (
                <Field>
                  <FieldLabel htmlFor={kindId}>{t("config.type")}</FieldLabel>
                  <Input id={kindId} value={kindLabel(provider.kind)} disabled readOnly />
                </Field>
              ) : group && (
                <Field>
                  <FieldLabel htmlFor={clusterChosen ? undefined : kindId}>{t("config.type")}</FieldLabel>
                  {/* The platform type first, as tabs; picking one moves to its first platform. */}
                  {/* On a tray, so the types not chosen read as choices too. */}
                  <Tabs
                    variant="pill"
                    color="default"
                    value={group.category}
                    onValueChange={(next) => chooseKind(groups.find((g) => g.category === next)?.kinds[0] ?? kind)}
                  >
                    <TabsList aria-label={t("table.column.category")} className="self-start">
                      {groups.map((g) => <TabItem key={g.category} value={g.category} label={groupLabel(g)} disabled={busy} />)}
                    </TabsList>
                  </Tabs>
                  {/* Then the vendor among that type's. A cluster is one platform: there is nothing more to pick. */}
                  {!clusterChosen && (
                    // One select per platform type: a select whose options are swapped under it resets its value to
                    // one of the old options. It also reports only a platform of this type as the choice.
                    <Select key={group.category} value={kind} onValueChange={(next) => { if (group.kinds.includes(next)) chooseKind(next); }} disabled={busy}>
                      <SelectTrigger id={kindId} placeholder={t("config.typeRequired")} className="w-full" />
                      <SelectContent>
                        {group.kinds.map((k) => <SelectItem key={k} value={k}>{kindLabel(k)}</SelectItem>)}
                      </SelectContent>
                    </Select>
                  )}
                </Field>
              )}
              {/* What adding this platform leads to: each resource it connects, and how that resource becomes compute nodes. */}
              {!provider && connects.length > 0 && (
                <div className="flex min-w-0 flex-col gap-1.5">
                  <span id={connectsId} className="px-1.5 text-body font-medium text-fg-muted">{t("config.connects")}</span>
                  <ul aria-labelledby={connectsId} className="m-0 flex list-none flex-col gap-1.5 px-1.5 py-0 text-label text-fg-subtle">
                    {connects.map((resource) => (
                      <li key={resource.type} className="flex items-baseline gap-2">
                        <span className="flex-none"><PlatformResourceBadge resource={resource} /></span>
                        <span>{t(kubernetes ? "config.howOwnCluster" : slurm ? "config.howSlurmCluster" : `config.how.${resource.type}`, { defaultValue: "" })}</span>
                      </li>
                    ))}
                  </ul>
                </div>
              )}
            </>
          )}
          {clusterChosen ? cluster.body : (
            <form id={formId} ref={formRef} noValidate onSubmit={formSubmit(form, () => formRef.current, submit)} className="flex flex-col gap-4">
              {requirement && <Notice tone="info" title={requirement} />}
              <FormField control={form.control} name="name" label={t("config.name")} required rules={{ validate: (value) => value.trim() !== "" || t("config.nameRequired") }}>
                {(binding) => <Input {...binding} disabled={locked} placeholder={t("config.namePlaceholder", { kind: selectedKind ? kindLabel(selectedKind.kind) : "provider" })} />}
              </FormField>
              {(selectedKind?.credential_fields ?? []).length > 0 && <h3 className="m-0 px-1.5 pt-1 text-body font-semibold text-fg-default">{t("config.credentials")}</h3>}
              {(selectedKind?.credential_fields ?? []).map((field) => (
                <CredentialFieldInput key={`${kind}:${field.extra ? "extra" : "base"}:${field.name}`} control={form.control} field={field} editing={editing} disabled={locked} />
              ))}
              {/* AutoDL's API lists Pro container instances only; other instances never appear, which would otherwise look like a broken key. */}
              {(provider?.kind ?? kind) === "autodl" && <p className={HINT}>{t("config.autodlProOnly")}</p>}
              {/* Only Secure Cloud pods are supported; say so before a Community Cloud pod fails to install. */}
              {(provider?.kind ?? kind) === "runpod" && <p className={HINT}>{t("config.runpodSecureOnly")}</p>}
              {editing && <p className={HINT}>{t("config.credentialsRetained")}</p>}
              {submitError && <Notice tone="danger" title={submitError} />}
            </form>
          )}
        </div>
        {/* The cluster form brings its own buttons: Connect, then Close once the command is shown. */}
        <DialogFooter className="mt-2">
          <div ref={footerRef} className="contents">
            {clusterChosen ? cluster.footer(close) : (
              <>
                <Button variant="secondary" onClick={startTest} disabled={!kind || submitting || testJob !== null}>{t("test.run")}</Button>
                <Button variant="ghost" onClick={close} disabled={submitting}>{t("config.cancel")}</Button>
                <Button type="submit" form={formId} variant="primary" loading={submitting || (editing && !editReady && !loadError)} disabled={!editReady || loadError || testJob !== null}>
                  {t(provider ? "config.save" : "config.add")}
                </Button>
              </>
            )}
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
    <ProviderTestDialog open={testJob !== null} run={testJob?.run ?? NOOP_TEST_RUN} onClose={() => setTestJob(null)} />
    </>
  );
}

const HINT = "m-0 px-1.5 text-label text-fg-subtle";

/** One credential field of the kind's schema: its label, placeholder and whether it is a secret are the server's. */
function CredentialFieldInput({ control, field, editing, disabled }: { control: Control<FormValues>; field: ProviderCredentialField; editing: boolean; disabled: boolean }) {
  const { t, i18n } = useTranslation("providers");
  const language = i18n.resolvedLanguage || i18n.language;
  const label = localizedText(field.label, language) || field.name;
  const placeholder = localizedText(field.placeholder, language) || undefined;
  // A saved secret is never shown, so an edit asks for one only to replace it.
  const required = !editing && field.required;
  return (
    <FormField
      control={control}
      name={credentialPath(field)}
      label={label}
      required={required}
      rules={{
        required: required ? t("config.fieldRequired", { field: label }) : false,
        validate: (value) => field.default_template !== undefined && typeof value === "string" && value.trim() === field.default_template.trim()
          ? t("config.templateUnchanged", { field: label })
          : true,
      }}
    >
      {(binding) => field.secret
        ? <PasswordInput {...binding} value={binding.value ?? ""} disabled={disabled} placeholder={placeholder} autoComplete="off" />
        : <Input {...binding} value={binding.value ?? ""} disabled={disabled} placeholder={placeholder}
          // Addresses, ids and keys are not prose: no spelling underline, no autocorrection.
          spellCheck={false} autoCapitalize="off" autoCorrect="off" />}
    </FormField>
  );
}

function credentialPath(field: ProviderCredentialField): CredentialPath {
  return (field.extra ? `extra.${field.name}` : field.name) as CredentialPath;
}

/** What a new account's form starts with: the non-secret defaults its kind declares. */
function credentialDefaults(info?: ProviderKindInfo): Partial<FormValues> {
  const values: Record<string, string> = {};
  const extra: Record<string, string> = {};
  for (const field of info?.credential_fields ?? []) {
    if (field.secret) continue;
    const value = field.default_template ?? field.default_value;
    if (value === undefined) continue;
    if (field.extra) extra[field.name] = value;
    else values[field.name] = value;
  }
  return { ...values, extra };
}

/** The request: the name and the kind's fields that hold something. The server keeps what a field left empty had. */
function requestOf(values: FormValues, fields: ProviderCredentialField[]) {
  const body: { name: string; endpoint?: string; api_key?: string; public_key?: string; private_key?: string; extra?: Record<string, string> } = { name: values.name };
  const extra: Record<string, string> = {};
  for (const field of fields) {
    const value = field.extra ? values.extra?.[field.name] : (values as unknown as Record<string, unknown>)[field.name];
    if (typeof value !== "string" || value === "") continue;
    if (field.extra) extra[field.name] = value;
    else (body as Record<string, unknown>)[field.name] = value;
  }
  if (Object.keys(extra).length > 0) body.extra = extra;
  return body;
}

/** What a connection test sends of the form: the fields that hold something, trimmed. A blank one is left out, not sent as an empty credential. */
function testValuesOf(values: FormValues): ProviderTestOverride {
  const req: ProviderTestOverride = {};
  for (const key of ["endpoint", "api_key", "public_key", "private_key"] as const) {
    const value = values[key].trim();
    if (value) req[key] = value;
  }
  const extra: Record<string, string> = {};
  for (const [name, value] of Object.entries(values.extra ?? {})) {
    if (typeof value === "string" && value.trim()) extra[name] = value.trim();
  }
  if (Object.keys(extra).length > 0) req.extra = extra;
  return req;
}

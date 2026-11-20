import { z } from "zod";
import type { SlurmCluster, SlurmClusterRequest, SlurmClusterUpdate, SlurmDefaults } from "../../slurmApi";

/**
 * An entry made from a cloud account's HPC cluster: what the form starts
 * with, and the source it records.
 */
export interface SlurmClusterPrefill {
  name: string;
  provider_id: number;
  provider_cluster_id: string;
  /** "Account · cluster" as the drawer names the source. */
  source_label: string;
}

const URL_PATTERN = /^\s*https?:\/\/\S+\s*$/i;

/** The form's fields. Only what the server states as required is checked here; the server judges the rest. */
export function formSchema(t: (key: string) => string) {
  return z.object({
    name: z.string().refine((value) => value.trim() !== "", t("form.nameRequired")),
    server_url: z.string()
      .refine((value) => value.trim() !== "", t("form.serverUrlRequired"))
      .refine((value) => value.trim() === "" || URL_PATTERN.test(value), t("form.serverUrlInvalid")),
    run_as: z.string(),
    account_user: z.string(),
    install_dir: z.string(),
    script_dir: z.string(),
    sbatch_args: z.string(),
    idle_return_seconds: z.number().nullable(),
    pending_timeout_seconds: z.number().nullable(),
  });
}
export type FormValues = z.infer<ReturnType<typeof formSchema>>;
export type FormField_ = keyof FormValues;

// The request fields a refused entry can name (SLURM_CLUSTER_INVALID's field)
// and the form field each one belongs to.
export const FIELD_OF: Record<string, FormField_> = {
  name: "name",
  server_url: "server_url",
  run_as: "run_as",
  account_user: "account_user",
  install_dir: "install_dir",
  script_dir: "script_dir",
  sbatch_args: "sbatch_args",
  idle_return_seconds: "idle_return_seconds",
  pending_timeout_seconds: "pending_timeout_seconds",
};

// The fields inside the collapsed "Advanced" section: a refusal naming one opens it.
export const ADVANCED_FIELDS: ReadonlySet<FormField_> = new Set<FormField_>([
  "run_as", "account_user", "install_dir", "script_dir", "sbatch_args", "idle_return_seconds", "pending_timeout_seconds",
]);

/**
 * A new entry's form starts empty: the server's defaults (GET /api/v1/slurm/defaults)
 * are shown as hints and a field left empty is not sent, so the server applies
 * them. Editing starts from the entry.
 */
export function defaultsOf(prefill?: SlurmClusterPrefill, edit?: SlurmCluster): FormValues {
  if (edit) {
    return {
      name: edit.name, server_url: edit.server_url, run_as: edit.run_as, account_user: edit.account_user,
      install_dir: edit.install_dir, script_dir: edit.script_dir, sbatch_args: edit.sbatch_args,
      idle_return_seconds: edit.idle_return_seconds, pending_timeout_seconds: edit.pending_timeout_seconds,
    };
  }
  return {
    name: prefill?.name ?? "", server_url: window.location.origin, run_as: "", account_user: "",
    install_dir: "", script_dir: "", sbatch_args: "", idle_return_seconds: null, pending_timeout_seconds: null,
  };
}

/**
 * Whether the typed server address is the platform's own temporary public
 * address (`quick`, from GET /slurm/defaults; "" when there is none): the
 * server says which address that is, this only compares what was typed with it.
 */
export function isQuickTunnelAddress(typed: string, quick: string | undefined): boolean {
  if (!quick) return false;
  const origin = (value: string) => { try { return new URL(value.trim()).origin.toLowerCase(); } catch { return ""; } };
  return origin(typed) !== "" && origin(typed) === origin(quick);
}

export function requestOf(values: FormValues, prefill?: SlurmClusterPrefill): SlurmClusterRequest {
  const request: SlurmClusterRequest = { name: values.name.trim(), server_url: values.server_url.trim() };
  if (values.idle_return_seconds !== null) request.idle_return_seconds = values.idle_return_seconds;
  if (values.pending_timeout_seconds !== null) request.pending_timeout_seconds = values.pending_timeout_seconds;
  if (prefill) {
    request.provider_id = prefill.provider_id;
    request.provider_cluster_id = prefill.provider_cluster_id;
  }
  const optional = { run_as: values.run_as, account_user: values.account_user, install_dir: values.install_dir, script_dir: values.script_dir, sbatch_args: values.sbatch_args } as const;
  for (const [key, value] of Object.entries(optional)) {
    const text = value.trim();
    if (text) (request as unknown as Record<string, string>)[key] = text;
  }
  return request;
}

/**
 * What an edit sends: only the fields that differ from the entry (an emptied
 * text is sent empty, which clears it; an empty number takes the server's
 * default). The server URL, the agents' account and the two directories are
 * fixed at creation and never are.
 */
export function updateOf(values: FormValues, entry: SlurmCluster, defaults: SlurmDefaults | null): SlurmClusterUpdate {
  const update: SlurmClusterUpdate = {};
  const name = values.name.trim();
  if (name !== entry.name) update.name = name;
  const account = values.account_user.trim();
  if (account !== entry.account_user) update.account_user = account;
  const sbatch = values.sbatch_args.trim();
  if (sbatch !== entry.sbatch_args) update.sbatch_args = sbatch;
  const idle = values.idle_return_seconds ?? defaults?.idle_return_seconds;
  if (idle !== undefined && idle !== entry.idle_return_seconds) update.idle_return_seconds = idle;
  const pending = values.pending_timeout_seconds ?? defaults?.pending_timeout_seconds;
  if (pending !== undefined && pending !== entry.pending_timeout_seconds) update.pending_timeout_seconds = pending;
  return update;
}

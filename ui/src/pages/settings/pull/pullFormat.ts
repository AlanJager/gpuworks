import { APIError, apiErrorText, type APIErrorTranslator } from "../../../apiError";
import type { NodeRequirementsMap } from "../../../api";

/**
 * hostOf is the registry host of an endpoint or a credential: the address
 * without its scheme, path and credentials, lower-cased. "registry.example/gpuworks/"
 * and "https://registry.example" both give "registry.example".
 */
export function hostOf(address: string): string {
  let rest = address.trim().toLowerCase().replace(/^[a-z][a-z0-9+.-]*:\/\//, "");
  rest = rest.split("/")[0] ?? "";
  const at = rest.lastIndexOf("@");
  return at >= 0 ? rest.slice(at + 1) : rest;
}

/** requirementPicks flattens a node selector into the "key=value" picks the selector control shows. */
export function requirementPicks(requirements: NodeRequirementsMap | undefined): string[] {
  return Object.entries(requirements ?? {}).flatMap(([key, values]) => values.map((value) => `${key}=${value}`));
}

/** picksToRequirements is the inverse: values of one key are alternatives, a pick without "=" is dropped. */
export function picksToRequirements(picks: string[]): NodeRequirementsMap {
  const out: NodeRequirementsMap = {};
  for (const raw of picks) {
    const split = raw.indexOf("=");
    if (split <= 0) continue;
    const key = raw.slice(0, split).trim();
    const value = raw.slice(split + 1).trim();
    if (!key || !value) continue;
    const values = out[key] ?? [];
    if (!values.includes(value)) out[key] = [...values, value];
  }
  return out;
}

/** requirementChips is one short text per selector key; an empty selector has none (it means every node). */
export function requirementChips(requirements: NodeRequirementsMap | undefined): string[] {
  return Object.entries(requirements ?? {})
    .filter(([, values]) => values.length > 0)
    .map(([key, values]) => `${key}=${values.join(" | ")}`);
}

/** kibToMiB shows a stored KiB/s limit in the MB/s unit the form uses (1 MB/s = 1024 KiB/s), without float noise. */
export function kibToMiB(kib: number): number {
  return Math.round((kib / 1024) * 100) / 100;
}

/** miBToKiB converts the form's MB/s back to the KiB/s the API stores; a positive limit is at least 1 KiB/s. */
export function miBToKiB(mib: number): number {
  return mib > 0 ? Math.max(1, Math.round(mib * 1024)) : 0;
}

/** moved returns list with the item at index shifted by delta (clamped); the same list when nothing moves. */
export function moved<T>(list: T[], index: number, delta: number): T[] {
  const target = index + delta;
  if (index < 0 || index >= list.length || target < 0 || target >= list.length) return list;
  const next = [...list];
  [next[index], next[target]] = [next[target], next[index]];
  return next;
}

export type PullTranslator = (key: string, options?: Record<string, unknown>) => string;

/**
 * pullErrorText is the sentence for a failed pull-settings request. A refused
 * input names the form field in the console's own words; everything else is
 * the product error's localized text.
 */
export function pullErrorText(tPull: PullTranslator, tCommon: APIErrorTranslator, error: unknown): string {
  if (error instanceof APIError && error.code === "PULL_SETTING_INVALID") {
    const field = String(error.params?.field ?? "");
    return tPull("pull.invalidField", {
      field: tPull(`pull.fields.${field}`, { defaultValue: field }),
      detail: String(error.params?.detail ?? error.fallback),
    });
  }
  return apiErrorText(tCommon, error);
}

/** pullErrorField is the form field a refused input belongs to, or "" for any other error. */
export function pullErrorField(error: unknown): string {
  return error instanceof APIError && error.code === "PULL_SETTING_INVALID" ? String(error.params?.field ?? "") : "";
}

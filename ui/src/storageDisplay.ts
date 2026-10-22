import type { TFunction } from "i18next";
import { APIError, apiErrorText } from "./apiError";

/** The type names a person reads: local storage, NFS, NFS with local cache, mounted shared directory. */
export function storageTypeLabel(t: TFunction, type?: string): string {
  if (!type) return "";
  return t(`storageType.${type}`, { ns: "common", defaultValue: type });
}

/** A node's state for a storage (ready, mounting, failed, unresponsive, unsupported, offline). */
export function storageStateLabel(t: TFunction, state: string): string {
  return t(`storageNodeState.${state}`, { ns: "common", defaultValue: state });
}

/** A reason code from the server (acquisition unavailable, location reasons) as text; unknown codes pass through. */
export function storageReasonText(t: TFunction, code?: string): string {
  if (!code) return "";
  return t(`storageReason.${code}`, { ns: "common", defaultValue: code });
}

/**
 * A failed storage action as text: the server's error in the person's language,
 * and for a storage a deployment still uses, which deployments (#id).
 */
export function storageErrorText(t: TFunction, error: unknown): string {
  const base = apiErrorText((key, options) => t(key, { ns: "common", ...options }), error);
  if (error instanceof APIError && error.code === "STORAGE_IN_USE") {
    const ids = Array.isArray(error.params?.deployment_ids) ? (error.params.deployment_ids as number[]) : [];
    if (ids.length > 0) return `${base} (${ids.map((id) => `#${id}`).join(", ")})`;
  }
  return base;
}

/** Share of a volume in use, 0 to 100, for a progress bar. */
export function usedPercent(total?: number, available?: number): number {
  if (!total) return 0;
  return Math.max(0, Math.min(100, Math.round(((total - (available ?? 0)) / total) * 100)));
}

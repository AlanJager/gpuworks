import { api, type ImageArchivePreview } from "../../api";
import { APIError } from "../../apiError";
import { outboundPolicyApi } from "../../outboundPolicyApi";

/** Hostname of an http(s) archive address, without port or userinfo. */
export function httpURLHost(raw: string): string {
  try {
    const url = new URL(raw.trim());
    if (url.protocol !== "http:" && url.protocol !== "https:") return "";
    return url.hostname;
  } catch {
    return "";
  }
}

/** Host the server says can be written to the allow list. Empty when it cannot. */
export function approvableHost(error: unknown): string {
  if (!(error instanceof APIError) || error.params?.approvable !== true) return "";
  const host = error.params.allow_host;
  return typeof host === "string" ? host.trim() : "";
}

/** The allow-list write failed before a product error was available. */
export class ArchiveAllowError extends Error {
  constructor() {
    super("ALLOW_HOST_FAILED");
    this.name = "ArchiveAllowError";
  }
}

async function allowPrivateHost(host: string): Promise<boolean> {
  const policy = await outboundPolicyApi.get();
  if (!policy.restrict_private_hosts) return false;
  const hosts = policy.allow_private_hosts.some((item) => item.toLowerCase() === host.toLowerCase())
    ? policy.allow_private_hosts
    : [...policy.allow_private_hosts, host];
  await outboundPolicyApi.put({
    restrict_private_hosts: policy.restrict_private_hosts,
    allow_private_hosts: hosts,
    revision: policy.revision,
  });
  return true;
}

/**
 * Resolves one archive URL.
 * When allow is set and the server says the host can be permitted, that host
 * is written once and the same address is resolved again.
 */
export async function previewArchiveURL(address: string, allow: boolean): Promise<ImageArchivePreview> {
  try {
    return await api.previewImageArchiveURL(address);
  } catch (error) {
    const host = approvableHost(error);
    if (!allow || !host) throw error;
    try {
      const wrote = await allowPrivateHost(host);
      if (!wrote) return await api.previewImageArchiveURL(address);
    } catch (failure) {
      if (failure instanceof APIError) throw failure;
      throw new ArchiveAllowError();
    }
    return await api.previewImageArchiveURL(address);
  }
}

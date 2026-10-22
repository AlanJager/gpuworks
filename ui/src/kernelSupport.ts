import type { EngineProfile } from "./api";

export interface KernelRelease {
  major: number;
  minor: number;
}

// A node reports whatever `uname -r` printed: the release "<major>.<minor>" is
// followed by a patch level and the distribution's own suffix, as in
// "4.18.0-553.69.1.13.gc220c6303994.el8.x86_64", "5.15.0-113-generic" or
// "5.10.0-307.0.0.210.ky10.aarch64". Only the first two numbers decide which
// kernel features exist, so that is all this reads. A string that does not
// start with them is not a release this code can reason about and yields null,
// which keeps callers silent instead of guessing.
export function parseKernelRelease(kernel: string | null | undefined): KernelRelease | null {
  const match = /^(\d{1,5})\.(\d{1,5})(?:\D|$)/.exec((kernel ?? "").trim());
  if (!match) return null;
  return { major: Number(match[1]), minor: Number(match[2]) };
}

function isOlder(release: KernelRelease, floor: KernelRelease): boolean {
  return release.major === floor.major ? release.minor < floor.minor : release.major < floor.major;
}

export interface KVDiskKernelShortfall {
  /** The lowest kernel floor this node misses, verbatim as the engine stated it. */
  required: string;
  /** The kernel the node reported, verbatim. */
  current: string;
}

// kvDiskKernelShortfall reports that a node's kernel is too old for KV disk
// offloading, using the floors the engines publish on /engines
// (kv_disk_min_kernel) rather than a release number of its own. It returns null
// - no warning - when the node reported no kernel, when the kernel does not
// parse, when no engine states a floor, or when the kernel clears every floor.
// With several floors stated, the lowest one the node misses is the one to
// report: it is the nearest the node has to get to unlock the feature at all.
export function kvDiskKernelShortfall(
  kernel: string | null | undefined,
  engines: readonly Pick<EngineProfile, "kv_disk_min_kernel">[] | null | undefined,
): KVDiskKernelShortfall | null {
  const release = parseKernelRelease(kernel);
  if (!release) return null;
  const missed = (engines ?? [])
    .map((engine) => ({ text: engine.kv_disk_min_kernel ?? "", floor: parseKernelRelease(engine.kv_disk_min_kernel) }))
    .filter((entry): entry is { text: string; floor: KernelRelease } => entry.floor !== null)
    .filter((entry) => isOlder(release, entry.floor))
    .sort((a, b) => (a.floor.major - b.floor.major) || (a.floor.minor - b.floor.minor));
  if (missed.length === 0) return null;
  return { required: missed[0].text, current: (kernel ?? "").trim() };
}

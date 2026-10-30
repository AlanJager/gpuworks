import type { Deployment } from "../../api";

/** The largest file whose requests the create dialog reads in the browser to list its models. */
export const FILE_MODEL_SCAN_MAX_BYTES = 50 * 1024 * 1024;

/**
 * A model a batch can be sent to: "running" serves now, "asleep" is slept by
 * the platform (a request wakes it), "asleepHeld" is slept by an operator or a
 * schedule (a request does not wake it); any other value is the state of a
 * deployment that is starting or in trouble.
 */
export interface BatchModelOption {
  name: string;
  state: string;
}

const ASLEEP = new Set(["asleep", "sleeping"]);

function rank(state: string): number {
  if (state === "running") return 0;
  if (state === "asleep") return 1;
  if (state === "asleepHeld") return 2;
  return 3;
}

function stateOf(d: Pick<Deployment, "state" | "runtime_state" | "sleep_held">): string {
  const state = d.state ?? d.runtime_state;
  if (ASLEEP.has(state)) return d.sleep_held ? "asleepHeld" : "asleep";
  return state;
}

/**
 * batchModelOptions are the served names of the deployments that are active
 * (wanted running), asleep ones included: the gateway holds a request for an
 * asleep deployment and wakes it. A name several deployments share shows the
 * state of the one that serves best. The order is the list's (newest first).
 */
export function batchModelOptions(deployments: Array<Pick<Deployment, "served_name" | "desired_state" | "state" | "runtime_state" | "sleep_held">>): BatchModelOption[] {
  const byName = new Map<string, string>();
  for (const d of deployments) {
    if (d.desired_state !== "active" || !d.served_name) continue;
    const state = stateOf(d);
    const known = byName.get(d.served_name);
    if (known === undefined || rank(state) < rank(known)) byName.set(d.served_name, state);
  }
  return Array.from(byName, ([name, state]) => ({ name, state }));
}

/** The distinct body.model values of a JSON Lines text, in first-seen order; lines that are not requests are the server's to report. */
export function modelsInJSONL(text: string): string[] {
  const seen = new Set<string>();
  for (const line of text.split("\n")) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    try {
      const model = (JSON.parse(trimmed) as { body?: { model?: unknown } } | null)?.body?.model;
      if (typeof model === "string" && model) seen.add(model);
    } catch { /* not a request line */ }
  }
  return Array.from(seen);
}

/**
 * modelsInFile lists the models the requests file names; null when the file is
 * over the scan limit or cannot be read, and the check is skipped.
 */
export async function modelsInFile(file: File): Promise<string[] | null> {
  if (file.size > FILE_MODEL_SCAN_MAX_BYTES) return null;
  try {
    return modelsInJSONL(await file.text());
  } catch {
    return null;
  }
}

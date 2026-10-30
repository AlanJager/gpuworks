import type { EngineProfile, Node } from "../../api";
import { deployDraftPayload } from "./deployHelpers";

type Obj = Record<string, unknown>;

const isObject = (value: unknown): value is Obj =>
  typeof value === "object" && value !== null && !Array.isArray(value);

/** The deployment an edit starts from: its draft's request and the fields the server locks. */
export interface EditBase {
  request: Obj;
  locked: readonly string[];
}

// stableStringify gives equal values one text whatever their key order.
export function stableStringify(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(stableStringify).join(",")}]`;
  if (isObject(value))
    return `{${Object.keys(value)
      .sort()
      .filter((key) => value[key] !== undefined)
      .map((key) => `${JSON.stringify(key)}:${stableStringify(value[key])}`)
      .join(",")}}`;
  return JSON.stringify(value) ?? "null";
}

const stated = (value: unknown) =>
  !(
    value == null ||
    value === "" ||
    value === 0 ||
    value === false ||
    (Array.isArray(value) && value.length === 0) ||
    (isObject(value) && Object.keys(value).length === 0)
  );

// editPayload is the body of an edit (and of the evaluation of one): the
// form's create-shaped draft, held to what the deployment is. A field the
// server locks travels exactly as the draft states it - the form's own
// reading of a placement or a node list is only a view of it - and a launch
// key the deployment stores but the form no longer states is removed
// explicitly, because an edit keeps whatever it does not mention.
export function editPayload(
  values: Obj,
  nodes: Node[],
  profile: EngineProfile | undefined,
  modalities: string[],
  base: EditBase,
): Obj | null {
  const body = deployDraftPayload(values, nodes, profile, modalities);
  if (!body) return null;
  if (values.name != null) body.name = values.name;
  const image = String(values.image ?? "").trim();
  if (image) body.image = image;
  else delete body.image;

  // The level is changed on the deployment's scheduling policy, not by an
  // edit: the edit states the stored one, so a preview plans with it.
  if (stated(base.request.service_level)) body.service_level = base.request.service_level;
  else delete body.service_level;

  const storedQuick = isObject(base.request.quick_params) ? base.request.quick_params : {};
  const quick = isObject(body.quick_params) ? body.quick_params : {};
  const unset = new Set(Array.isArray(body.quick_params_unset) ? (body.quick_params_unset as string[]) : []);
  for (const key of Object.keys(storedQuick)) if (!(key in quick)) unset.add(key);
  if (unset.size) body.quick_params_unset = [...unset];

  // The form owns tp, pp and the GPU list of a placement; the rest of the
  // stored placement (a multi-node deployment's units) is the deployment's.
  const storedPlacement = isObject(base.request.placement) ? base.request.placement : {};
  if (Array.isArray(storedPlacement.units))
    body.placement = { ...((body.placement as Obj | undefined) ?? {}), units: storedPlacement.units };

  for (const field of base.locked) {
    const was = base.request[field];
    if (stated(was)) body[field] = structuredClone(was);
    else delete body[field];
  }
  return body;
}

// flatten reads a payload as comparable (field, value) rows: the launch
// configuration and the container overrides one row per key, so a change is
// counted where it was made.
function flatten(payload: Obj): Map<string, string> {
  const rows = new Map<string, string>();
  const nested = new Set(["quick_params", "quick_params_unset", "container_overrides", "node_requirements", "placement"]);
  for (const [field, value] of Object.entries(payload))
    if (!nested.has(field) && value !== undefined) rows.set(field, stableStringify(value));
  if (isObject(payload.quick_params))
    for (const [key, value] of Object.entries(payload.quick_params))
      rows.set(`quick_params.${key}`, stableStringify(value));
  if (Array.isArray(payload.quick_params_unset))
    for (const key of payload.quick_params_unset as string[]) rows.set(`quick_params.${key}`, "unset");
  const overrides = isObject(payload.container_overrides) ? payload.container_overrides : {};
  for (const [key, value] of Object.entries(overrides)) {
    if (key === "env" && isObject(value))
      for (const [name, text] of Object.entries(value)) rows.set(`container_overrides.env.${name}`, stableStringify(text));
    else rows.set(`container_overrides.${key}`, stableStringify(value));
  }
  for (const group of ["node_requirements", "placement"] as const) {
    const part = payload[group];
    if (isObject(part))
      for (const [key, value] of Object.entries(part)) rows.set(`${group}.${key}`, stableStringify(value));
  }
  return rows;
}

// payloadChanges names the fields where two payloads differ, so the submit
// bar can say how many changes an edit holds before the server plans them.
export function payloadChanges(base: Obj | null, current: Obj | null): string[] {
  if (!base || !current) return [];
  const was = flatten(base);
  const now = flatten(current);
  const fields = new Set([...was.keys(), ...now.keys()]);
  return [...fields].filter((field) => was.get(field) !== now.get(field)).sort();
}

// changedFormValues are the form fields whose value differs from the
// baseline, with the baseline's own fields the form no longer holds as
// undefined: what the deployer edited, to put back over a fresh draft.
export function changedFormValues(current: Obj, baseline: Obj): Obj {
  const out: Obj = {};
  for (const key of new Set([...Object.keys(current), ...Object.keys(baseline)]))
    if (stableStringify(current[key]) !== stableStringify(baseline[key])) out[key] = current[key];
  return out;
}

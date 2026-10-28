import type { EngineProfile, PdModeProfile, PdRoleName } from "../../api";

// Decode first, then prefill: the order the server's evaluate/deployment DTOs
// use, and the order the deploy form renders the two role sections in.
export const PD_ROLE_ORDER: PdRoleName[] = ["decode", "prefill"];

// PdFollowerDraft is one headless rank of a multi-node unit: another node
// (plus optional GPUs) that runs alongside the unit's own node, in rank
// order. It carries no TP/PP of its own - those live once on the unit's
// role (PdRoleDraft.pp) and the role's existing tp.
export interface PdFollowerDraft {
  node_uuid?: string;
  gpus?: number[];
}

export interface PdUnitDraft {
  node_uuid?: string;
  gpus?: number[];
  followers?: PdFollowerDraft[];
}

export interface PdRoleDraft {
  // The InputNumber control normally holds a number; typed as number | string
  // so a form draft mid-edit (a cleared field briefly reads "") type-checks
  // without a cast.
  tp?: number | string;
  // Pipeline-parallel degree. Optional: most roles run PP 1 (implicit,
  // omitted from the wire); a multi-node unit still needs the role's TP
  // stated (enforced per unit, not here - see unitNeedsTP).
  pp?: number | string;
  units?: PdUnitDraft[];
}

// unitHasFollowers/unitNeedsTP are the one place the UI decides a unit is
// "multi-node": any unit with at least one follower row, regardless of
// whether that row is filled in yet - the same "count what the screen
// shows" rule buildPdPlacement already follows for units themselves.
export function unitHasFollowers(unit: PdUnitDraft | undefined): boolean {
  return Boolean(unit?.followers && unit.followers.length > 0);
}

export type PdRolesDraft = Partial<Record<PdRoleName, PdRoleDraft>>;

// pdModesForEngine reads the PD mode catalog straight off the engine profile
// the deploy form already loaded (ADR 0028's pdmode package, served as
// EngineProfile.pd_modes) - the server is the sole source for which modes
// exist, which vendors each runs on and the rules the form applies.
export function pdModesForEngine(profile?: Pick<EngineProfile, "pd_modes"> | null): PdModeProfile[] {
  return profile?.pd_modes ?? [];
}

// pdModesForVendor narrows the engine's modes to the ones declared for the
// draft's vendor (vllm offers both NVIDIA and Ascend modes); with no vendor
// chosen yet it is the whole catalog.
export function pdModesForVendor(
  profile: Pick<EngineProfile, "pd_modes"> | null | undefined,
  vendor?: string,
): PdModeProfile[] {
  const modes = pdModesForEngine(profile);
  return vendor ? modes.filter((mode) => mode.vendors.includes(vendor)) : modes;
}

// pdModeOf finds one mode of the engine's catalog by name.
export function pdModeOf(
  profile: Pick<EngineProfile, "pd_modes"> | null | undefined,
  name: unknown,
): PdModeProfile | undefined {
  return pdModesForEngine(profile).find((mode) => mode.name === name);
}

// defaultPdMode picks the mode to preselect when a draft first switches to
// the PD topology. An engine can offer more than one mode (vllm offers modes
// for NVIDIA and for Ascend): it prefers the first mode declared for the
// chosen vendor, then the first verified mode, then the first. The mode
// Select still lets the deployer override this pick.
export function defaultPdMode(
  profile: Pick<EngineProfile, "pd_modes"> | undefined,
  vendor?: string,
): string | undefined {
  const modes = pdModesForEngine(profile);
  const pick =
    (vendor ? modes.find((mode) => mode.vendors.includes(vendor)) : undefined) ??
    modes.find((mode) => !mode.unverified) ??
    modes[0];
  return pick?.name;
}

// pdPolicyValue keeps a policy the deployer chose while the mode still allows
// it, and otherwise takes the mode's default (the first option); a mode with
// no options for the policy has no value.
export function pdPolicyValue(options: string[] | undefined, current: unknown): string | undefined {
  if (!options?.length) return undefined;
  return typeof current === "string" && options.includes(current) ? current : options[0];
}

// pdRoleRemovable reports whether a role's unit row may be removed: a role the
// mode may scale to zero may lose its last unit, any other keeps one.
export function pdRoleRemovable(mode: PdModeProfile | undefined, role: PdRoleName, units: number): boolean {
  return units > 1 || Boolean(mode?.scalable_to_zero.includes(role));
}

// pdActive reports whether the draft's topology choice is PD disaggregation.
// This is the one place that reads node_selection_mode for that question;
// every other PD helper takes the answer as a parameter or calls this.
export function pdActive(values: Record<string, unknown>): boolean {
  return values.node_selection_mode === "pd";
}

// unitsPlacementAuto reports whether the draft is a multi-node or PD deployment
// whose nodes the platform picks (placement_mode "auto" on the wire). Both
// shapes default to it: an unset units_placement field reads as "auto", and
// "manual" is the deployer's (or a stored deployment's) choice to name the
// nodes. Single-node drafts have their own automatic/manual position
// (node_selection_mode).
export function unitsPlacementAuto(values: Record<string, unknown>): boolean {
  const units = pdActive(values) || Number(values.nnodes || 1) > 1;
  return units && values.units_placement !== "manual";
}

// defaultPdRoles seeds a fresh PD draft: one unit per role, TP 1. The
// deployer fills in nodes and TP from there.
export function defaultPdRoles(): PdRolesDraft {
  return {
    decode: { tp: 1, units: [{}] },
    prefill: { tp: 1, units: [{}] },
  };
}

// buildPdPlacement projects the pd_* draft fields onto the wire
// `placement.pd` object (ADR 0028). It states exactly the rows the deployer
// sees - an incomplete unit (no node picked yet) still counts, so a debounced
// evaluate mid-edit reports the same shape the screen shows, and the field
// validation lands on the row that is actually missing something. An
// automatically placed draft (unitsPlacementAuto) states its units without
// node, GPUs or followers: the platform picks them, one node per unit, and
// keeps the role's unit count and TP/PP the deployer set. Returns
// undefined when PD is not the draft's topology, or its mode is unset, so
// callers omit the key rather than send an empty shell.
export function buildPdPlacement(
  values: Record<string, unknown>,
): Record<string, unknown> | undefined {
  if (!pdActive(values)) return undefined;
  const mode = String(values.pd_mode || "").trim();
  if (!mode) return undefined;
  const rolesDraft = (values.pd_roles || {}) as PdRolesDraft;
  const auto = unitsPlacementAuto(values);
  const roles: Record<string, unknown> = {};
  for (const role of PD_ROLE_ORDER) {
    const draft = rolesDraft[role];
    const units = (draft?.units || []).map((unit) => {
      const out: Record<string, unknown> = {};
      if (auto) return out;
      if (unit?.node_uuid) out.node_uuid = unit.node_uuid;
      if (Array.isArray(unit?.gpus) && unit.gpus.length) out.gpus = unit.gpus;
      if (Array.isArray(unit?.followers) && unit.followers.length) {
        out.followers = unit.followers.map((follower) => {
          const followerOut: Record<string, unknown> = {};
          if (follower?.node_uuid) followerOut.node_uuid = follower.node_uuid;
          if (Array.isArray(follower?.gpus) && follower.gpus.length) followerOut.gpus = follower.gpus;
          return followerOut;
        });
      }
      return out;
    });
    if (!units.length) continue;
    const roleOut: Record<string, unknown> = { units };
    if (draft?.tp != null && draft.tp !== "") roleOut.tp = draft.tp;
    if (draft?.pp != null && draft.pp !== "") roleOut.pp = draft.pp;
    roles[role] = roleOut;
  }
  if (!Object.keys(roles).length) return undefined;
  const pd: Record<string, unknown> = { mode, roles };
  const fallback = String(values.pd_fallback || "").trim();
  if (fallback) pd.fallback = fallback;
  const kvLoadFailure = String(values.pd_kv_load_failure || "").trim();
  if (kvLoadFailure) pd.kv_load_failure = kvLoadFailure;
  return pd;
}

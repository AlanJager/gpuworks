import type { EvaluateResponse } from "../../api";

// Evaluation returns a display projection, not a new user override. Keep its
// provenance through the feedback loop so an earlier default cannot defeat a
// newly selected template (ADR 0020). Creation still confirms the complete
// evaluated form; this filter only prepares another evaluation.
export class EvaluateQuickParams {
  private displayed: Record<string, unknown> = {};
  private authored = new Set<string>();

  noteChanged(changed: Record<string, unknown>) {
    for (const key of Object.keys(changed)) this.authored.add(key);
  }

  prepare(payload: Record<string, unknown>): Record<string, unknown> {
    const quick = (payload.quick_params ?? {}) as Record<string, unknown>;
    // Cleared controls and disabled tiers state no value. Their explicit
    // tombstones already travel in quick_params_unset, which is left intact.
    for (const key of this.authored) {
      if (!Object.prototype.hasOwnProperty.call(quick, key)) this.authored.delete(key);
    }
    const explicit: Record<string, unknown> = {};
    for (const [key, value] of Object.entries(quick)) {
      // Programmatic controls also edit the form, without onValuesChange.
      if (
        !Object.prototype.hasOwnProperty.call(this.displayed, key) ||
        JSON.stringify(value) !== JSON.stringify(this.displayed[key])
      ) this.authored.add(key);
      if (this.authored.has(key)) explicit[key] = value;
    }
    const request = { ...payload };
    if (Object.keys(explicit).length) request.quick_params = explicit;
    else delete request.quick_params;
    return request;
  }

  accept(response: EvaluateResponse) {
    if (!response.form_values) return;
    // Do this before updating the form, and only after the request's stale
    // guards pass. Its programmatic update must not look like a user edit.
    this.displayed = structuredClone(response.form_values);
    // Once an explicit extra_args flag moves into its field, the field owns
    // that choice. Editing the remaining argument text must not erase it.
    for (const moved of response.moved ?? []) {
      if (moved.source === "extra_args") this.authored.add(moved.key);
    }
  }
}

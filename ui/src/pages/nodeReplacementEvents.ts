import type { TFunction } from "i18next";

import type { InstanceEvent } from "../api";

/**
 * The timeline entries (kind "automation") of a replica moving off a node
 * that stayed offline or is draining. Their params are names and lists, not
 * counts, so they are worded here rather than by the lifecycle mapping.
 */
export const nodeReplacementEventCodes = [
  "NODE_REPLACED",
  "NODE_REPLACE_REVERTED",
  "NODE_REPLACE_NO_CANDIDATE",
  "NODE_DRAIN_MOVED",
] as const;

const known = new Set<string>(nodeReplacementEventCodes);

export function isNodeReplacementEvent(code: string): boolean {
  return known.has(code);
}

function text(event: InstanceEvent, name: string): string {
  const value = event.params?.[name];
  return typeof value === "string" ? value.trim() : "";
}

function num(event: InstanceEvent, name: string): number | undefined {
  const value = event.params?.[name];
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function list(event: InstanceEvent, name: string): string[] {
  const value = event.params?.[name];
  return Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : [];
}

// duration words a span of seconds the way the order form counts it: whole
// days, then hours, then minutes.
function durationText(t: TFunction<"deployments">, seconds: number): string {
  if (seconds >= 86400 && seconds % 86400 === 0) return t("nodeReplacement.duration.days", { value: seconds / 86400 });
  if (seconds >= 3600 && seconds % 3600 === 0) return t("nodeReplacement.duration.hours", { value: seconds / 3600 });
  if (seconds < 60) return t("nodeReplacement.duration.seconds", { value: Math.round(seconds) });
  return t("nodeReplacement.duration.minutes", { value: Math.round(seconds / 60) });
}

// what names the replica a node move is about: its number (with the PD role),
// or just "a replica" when the event names none.
function what(t: TFunction<"deployments">, event: InstanceEvent): string {
  const unit = num(event, "unit");
  if (unit == null) return t("nodeReplacement.what.replica");
  const role = text(event, "role");
  const label = role ? `${t(`detail.pd.role.${role}`, { defaultValue: role })} ${unit}` : String(unit);
  return t("nodeReplacement.what.unit", { label });
}

/** Word one node replacement entry from its code and params. */
export function nodeReplacementEventText(t: TFunction<"deployments">, event: InstanceEvent, code: string): string {
  const to = text(event, "to");
  const after = num(event, "after_sec");
  const rendered = t(`instance.event.${code}`, {
    from: text(event, "from"),
    to,
    target: to ? t("nodeReplacement.movedTo", { node: to }) : t("nodeReplacement.movedElsewhere"),
    after: after != null ? durationText(t, after) : "",
    what: what(t, event),
    nodes: list(event, "nodes").join(", "),
    defaultValue: "",
  });
  return rendered.trim() || event.message?.trim() || code;
}

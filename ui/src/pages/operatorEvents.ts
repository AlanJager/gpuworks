import type { TFunction } from "i18next";

import type { InstanceEvent } from "../api";

/**
 * The deployment timeline entries an operator's order leaves (kind
 * "schedule"): a changed replica count and a sleep or wake order. Their
 * params are counts, lists and names, so they are worded here rather than by
 * the lifecycle mapping, whose from/to are memory sizes.
 */
export const operatorEventCodes = ["REPLICAS_CHANGED", "SLEEP_ORDERED", "WAKE_ORDERED"] as const;

const known = new Set<string>(operatorEventCodes);

export function isOperatorEvent(code: string): boolean {
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

/** Word one operator order from its code and params. */
export function operatorEventText(t: TFunction<"deployments">, event: InstanceEvent, code: string): string {
  const role = text(event, "role");
  const roleLabel = role ? t(`detail.pd.role.${role}`, { defaultValue: role }) : "";
  const scope = text(event, "scope");
  const instances = list(event, "instances").map((id) => id.slice(0, 8)).join(", ");
  const rendered = t(`instance.event.${code}`, {
    from: num(event, "from") ?? "",
    to: num(event, "to") ?? "",
    roleNote: roleLabel ? t("operator.roleNote", { role: roleLabel }) : "",
    level: num(event, "level") ?? "",
    scope: scope ? t(`operator.scope.${scope}`, { role: roleLabel, instances, defaultValue: scope }) : "",
    modeNote: text(event, "mode") === "abort" ? t("operator.abortNote") : "",
    instances,
    defaultValue: "",
  });
  return rendered.trim() || event.message?.trim() || code;
}

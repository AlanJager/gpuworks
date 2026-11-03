import type { TFunction } from "i18next";

import type { InstanceEvent } from "../api";
import { paramBool as bool, paramList as list, paramNumber as num, paramText as text } from "./eventParams";

/**
 * The deployment timeline entries an operator's order leaves (kind
 * "schedule"): a changed replica count, a sleep or wake order, and letting
 * requests wake a held sleep. Their params are counts, lists and names, so
 * they are worded here rather than by the lifecycle mapping, whose from/to
 * are memory sizes.
 */
export const operatorEventCodes = ["REPLICAS_CHANGED", "SLEEP_ORDERED", "WAKE_ORDERED", "SLEEP_HOLD_RELEASED"] as const;

const known = new Set<string>(operatorEventCodes);

export function isOperatorEvent(code: string): boolean {
  return known.has(code);
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
    wakeNote: wakeNote(t, bool(event, "wake_on_request")),
    instances,
    defaultValue: "",
  });
  return rendered.trim() || event.message?.trim() || code;
}

// wakeNote says whether a request wakes what a sleep order put to sleep;
// nothing for an order recorded before the choice was.
function wakeNote(t: TFunction<"deployments">, wakeOnRequest: boolean | undefined): string {
  if (wakeOnRequest === undefined) return "";
  return wakeOnRequest ? t("operator.wakesOnRequestNote") : t("operator.heldNote");
}

import type { TFunction } from "i18next";

import type { InstanceEvent } from "../api";

/**
 * The scheduling center's timeline entries (kind "automation"): what a
 * schedule, idle sleep or a waiting wake did to the deployment. Their params
 * are lists and names, not counts, so they are worded here rather than by the
 * lifecycle mapping. The node-replacement entries are worded in
 * nodeReplacementEvents.ts.
 */
export const automationEventCodes = [
  "AUTO_SLEEP",
  "AUTO_WAKE",
  "AUTO_SCALE",
  "WAKE_WAITING",
  "WAKE_WAIT_TIMEOUT",
  "SCHEDULE_ACTION_FAILED",
  "YIELDED_FOR_WAKE",
] as const;

const known = new Set<string>(automationEventCodes);

export function isAutomationEvent(code: string): boolean {
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

// duration words a span of seconds the way the order forms count it: whole
// days, then hours, then minutes.
export function durationText(t: TFunction<"deployments">, seconds: number): string {
  if (seconds >= 86400 && seconds % 86400 === 0) return t("automation.duration.days", { value: seconds / 86400 });
  if (seconds >= 3600 && seconds % 3600 === 0) return t("automation.duration.hours", { value: seconds / 3600 });
  if (seconds < 60) return t("automation.duration.seconds", { value: Math.round(seconds) });
  return t("automation.duration.minutes", { value: Math.round(seconds / 60) });
}

// shortIDs names instances by the first characters of their uuid, as the
// instance panels do.
function shortIDs(ids: string[]): string {
  return ids.map((id) => id.slice(0, 8)).join(", ");
}

// preemptCause words a deployment giving its GPUs to another by service level:
// who got them, why (idle past its hold time, rank, a time-share turn) and
// whether requests in flight were cut off.
function preemptCause(t: TFunction<"deployments">, event: InstanceEvent): string {
  const target = num(event, "for_deployment");
  const who = target != null ? t("automation.preempt.who", { id: target }) : t("automation.preempt.someone");
  const reason = text(event, "reason");
  const why = t(`automation.preempt.${reason}`, { who, defaultValue: "" }) || t("automation.preempt.other", { who });
  return event.params?.abort === true ? why + t("automation.preempt.abort") : why;
}

/** Word one automation entry from its code and params. */
export function automationEventText(t: TFunction<"deployments">, event: InstanceEvent, code: string): string {
  const trigger = text(event, "trigger");
  const level = num(event, "level") ?? "";
  const instances = shortIDs(list(event, "instances"));
  const idle = num(event, "idle_sec");
  const preempt = trigger === "preempt";
  const cause = preempt
    ? preemptCause(t, event)
    : trigger ? t(`automation.cause.${trigger}`, { defaultValue: "", idle: idle != null ? durationText(t, idle) : "" }) : "";
  const waited = num(event, "waited_sec");
  const yielded = list(event, "yielded");
  const action = text(event, "action");
  const role = text(event, "role");
  // A failed sleep ordered to give GPUs to another deployment is not a
  // scheduled one: it has its own wording.
  const key = code === "SCHEDULE_ACTION_FAILED" && preempt ? `${code}_preempt` : code;
  const rendered = t(`instance.event.${key}`, {
    cause,
    level,
    instances,
    replicas: num(event, "replicas") ?? "",
    roleNote: role ? t("operator.roleNote", { role: t(`detail.pd.role.${role}`, { defaultValue: role }) }) : "",
    waited: waited != null ? durationText(t, waited) : "",
    yielded: yielded.length > 0 ? t("automation.yieldedNote", { names: yielded.join(", ") }) : "",
    action: action ? t(`automation.action.${action}`, { defaultValue: action }) : "",
    reason: text(event, "reason"),
    deployment: text(event, "deployment"),
    defaultValue: "",
  });
  return rendered.trim() || event.message?.trim() || code;
}

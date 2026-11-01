import type { TFunction } from "i18next";
import type { Deployment } from "../../api";
import { durationText } from "../durationText";
import { type YieldParams, savedPolicy } from "./model";

// spanText words a parameter in seconds the way the editor's fields count it
// (durationText's exact wording: nothing typed is rounded).
const spanText = (t: TFunction<"deployments">, seconds: number) => durationText(t, seconds, "exact");

// paramsSummary is the one line for a policy: its name and, for one that
// yields, the idle time and (for timeslice) the longest turn. It reads the saved
// parameters alone, so it needs no dictionary.
export function paramsSummary(t: TFunction<"deployments">, held: YieldParams): string {
  const name = t(`yieldPolicy.policy.name.${held.policy}`);
  const parts: string[] = [];
  if (held.policy === "never") return name;
  if (held.idle_sec !== undefined) {
    const key = held.policy === "idle" ? "yieldPolicy.summary.idleYield" : "yieldPolicy.summary.idle";
    parts.push(t(key, { value: spanText(t, held.idle_sec) }));
  }
  if (held.policy === "timeslice" && held.max_serve_sec !== undefined) {
    parts.push(t("yieldPolicy.summary.turn", { value: spanText(t, held.max_serve_sec) }));
  }
  if (parts.length === 0) return name;
  return t("yieldPolicy.summary.line", { policy: name, digest: parts.join(t("yieldPolicy.summary.separator")) });
}

// yieldSummary is the phrase for what a deployment holds.
export function yieldSummary(t: TFunction<"deployments">, dep: Deployment): string {
  return paramsSummary(t, savedPolicy(dep));
}

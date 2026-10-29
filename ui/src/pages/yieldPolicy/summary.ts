import type { TFunction } from "i18next";
import type { Deployment } from "../../api";
import { type YieldParams, applicable, draftOfParams, paramsOfDraft, presetOfDraft, savedPolicy } from "./model";

// spanText words a span of seconds the way the editor's fields count it:
// whole days, hours or minutes when it divides evenly, else seconds.
export function spanText(t: TFunction<"deployments">, seconds: number): string {
  if (seconds >= 86400 && seconds % 86400 === 0) return t("automation.duration.days", { value: seconds / 86400 });
  if (seconds >= 3600 && seconds % 3600 === 0) return t("automation.duration.hours", { value: seconds / 3600 });
  if (seconds >= 60 && seconds % 60 === 0) return t("automation.duration.minutes", { value: seconds / 60 });
  return t("automation.duration.seconds", { value: seconds });
}

// paramsSummary is the one phrase for a policy: a preset by its name, a custom
// one by "Custom", its policy and a digest of the parameters that matter
// (idle time, sleep level, automatic sleep).
export function paramsSummary(t: TFunction<"deployments">, held: YieldParams): string {
  const draft = draftOfParams(held);
  const preset = presetOfDraft(draft);
  if (preset !== "custom") return t(`yieldPolicy.preset.name.${preset}`);
  const params = paramsOfDraft(draft);
  const use = applicable(draft);
  const parts: string[] = [];
  if (use.idleSec && params.idle_sec !== undefined) parts.push(t("yieldPolicy.summary.idle", { value: spanText(t, params.idle_sec) }));
  if (use.sleepLevel && params.sleep_level) parts.push(t(`yieldPolicy.summary.level.${params.sleep_level}`));
  if (params.auto_sleep) parts.push(t("yieldPolicy.summary.autoSleep"));
  const policy = t(`yieldPolicy.policy.name.${params.policy}`);
  if (parts.length === 0) return t("yieldPolicy.summary.custom", { policy });
  return t("yieldPolicy.summary.customDigest", { policy, digest: parts.join(t("yieldPolicy.summary.separator")) });
}

// yieldSummary is the phrase for what a deployment holds.
export function yieldSummary(t: TFunction<"deployments">, dep: Deployment): string {
  return paramsSummary(t, savedPolicy(dep));
}

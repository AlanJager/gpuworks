import type { TFunction } from "i18next";
import type { CardCheckState } from "../../api";

// Wording for card checks. Every value here comes from the server (check ids,
// states, reasons); this only names them in the reader's language.

export function checkName(t: TFunction, id: string): string {
  return t(`cardHealth.check.${id}`, { ns: "nodes", defaultValue: id });
}

export function checkHelp(t: TFunction, id: string): string {
  return t(`cardHealth.checkHelp.${id}`, { ns: "nodes", defaultValue: "" });
}

export function stateName(t: TFunction, state: CardCheckState | "stale"): string {
  return t(`cardHealth.state.${state}`, { ns: "nodes", defaultValue: state });
}

/** Why a check warned, failed or was skipped, in words. */
export function reasonText(t: TFunction, state: CardCheckState, reason?: string): string {
  if (!reason) return "";
  if (state === "skip") return t(`cardHealth.skip.${reason}`, { ns: "nodes", defaultValue: reason });
  if (reason === "foreign_processes") return t("cardHealth.reason.foreign_processes", { ns: "nodes" });
  return t(`gpuHealth.code.${reason}.label`, { ns: "common", defaultValue: reason });
}

export function periodText(t: TFunction, seconds: number): string {
  const minutes = Math.round(seconds / 60);
  return minutes <= 1 ? t("cardHealth.period.minute", { ns: "nodes" }) : t("cardHealth.period.minutes", { ns: "nodes", count: minutes });
}

export function durationText(t: TFunction, seconds: number): string {
  if (seconds < 60) return t("cardHealth.duration.seconds", { ns: "nodes", count: Math.max(0, Math.round(seconds)) });
  if (seconds < 3600) return t("cardHealth.duration.minutes", { ns: "nodes", count: Math.floor(seconds / 60) });
  if (seconds < 86400) return t("cardHealth.duration.hours", { ns: "nodes", count: Math.floor(seconds / 3600) });
  return t("cardHealth.duration.days", { ns: "nodes", count: Math.floor(seconds / 86400) });
}

export function cardName(t: TFunction, index: number | undefined, fallback: string): string {
  return index !== undefined && index >= 0 ? t("cardHealth.card", { ns: "nodes", index }) : fallback;
}

export function localTime(iso?: string): string {
  if (!iso) return "-";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}

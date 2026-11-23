// General display formatting shared by every page.

import type { TFunction } from "i18next";

const BYTE_UNITS = ["B", "KiB", "MiB", "GiB", "TiB"] as const;
type ByteUnit = (typeof BYTE_UNITS)[number];

export interface FormatBytesOptions {
  /** Whole numbers from 10 up ("12 GiB", "9.5 GiB"), as progress lines show them; otherwise one decimal for every unit above B ("12.0 GiB"). */
  compact?: boolean;
  /** Smallest unit shown: with "KiB" even 100 bytes read "0.1 KiB". */
  minUnit?: ByteUnit;
  /** Largest unit shown: with "MiB" a gigabyte reads "1024.0 MiB". */
  maxUnit?: ByteUnit;
}

/** Human size with binary units; "0 B" for an unknown size. */
export function formatBytes(bytes?: number, options: FormatBytesOptions = {}): string {
  const lowest = BYTE_UNITS.indexOf(options.minUnit ?? "B");
  const highest = BYTE_UNITS.indexOf(options.maxUnit ?? "TiB");
  if (!bytes && lowest === 0) return "0 B";
  let value = (bytes ?? 0) / 1024 ** lowest;
  let unit = lowest;
  while (value >= 1024 && unit < highest) {
    value /= 1024;
    unit += 1;
  }
  const text = options.compact && (value >= 10 || unit === 0) ? String(Math.round(value)) : value.toFixed(unit === 0 ? 0 : 1);
  return `${text} ${BYTE_UNITS[unit]}`;
}

/** "3 min ago" for a number of seconds; coarse on purpose, callers that need a verdict take it from the server. */
export function agoText(t: TFunction, seconds: number): string {
  if (seconds < 60) return t("ago.now", { ns: "common" });
  if (seconds < 3600) return t("ago.minutes", { ns: "common", value: Math.floor(seconds / 60) });
  if (seconds < 86400) return t("ago.hours", { ns: "common", value: Math.floor(seconds / 3600) });
  return t("ago.days", { ns: "common", value: Math.floor(seconds / 86400) });
}

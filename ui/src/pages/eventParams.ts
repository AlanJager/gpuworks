import type { InstanceEvent } from "../api";

// Readers of a timeline entry's params, which differ by event code and arrive
// as plain JSON: each takes what is there when it has the expected kind and
// gives the empty value otherwise, so a wording never shows a stray value.

/** paramText is a string param, trimmed; "" when absent or not a string. */
export function paramText(event: InstanceEvent, name: string): string {
  const value = event.params?.[name];
  return typeof value === "string" ? value.trim() : "";
}

/** paramNumber is a finite number param; undefined otherwise. */
export function paramNumber(event: InstanceEvent, name: string): number | undefined {
  const value = event.params?.[name];
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

/** paramList is the strings of a list param; [] when absent or not a list. */
export function paramList(event: InstanceEvent, name: string): string[] {
  const value = event.params?.[name];
  return Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : [];
}

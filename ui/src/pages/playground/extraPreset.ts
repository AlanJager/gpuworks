function plainObject(value: unknown): value is Record<string, unknown> {
  return !!value && !Array.isArray(value) && typeof value === "object";
}

// mergedExtraJSON folds a preset into the current extra-parameter object.
// Nested objects merge one level, so a field inside chat_template_kwargs (the
// thinking switch, whose name is the model family's and resolved by the
// server) replaces only that field and leaves sibling template kwargs in place.
// A non-object or invalid document is rejected instead of being overwritten.
export function mergedExtraJSON(current: string, preset: Record<string, unknown>): string {
  const trimmed = current.trim();
  const parsed: unknown = trimmed ? JSON.parse(trimmed) : {};
  if (!plainObject(parsed)) throw new Error("extra parameters must be a JSON object");
  const merged: Record<string, unknown> = { ...parsed };
  for (const [key, value] of Object.entries(preset)) {
    const existing = merged[key];
    merged[key] = plainObject(value) && plainObject(existing) ? { ...existing, ...value } : value;
  }
  return JSON.stringify(merged, null, 2);
}

export type LocalizedText = string | Record<string, string>;

function normalizeLanguage(language: string): string {
  return language.trim().replace(/_/g, "-").toLowerCase();
}

/** Resolve a server-owned multilingual string for the current UI language. */
export function localizedText(value: LocalizedText | undefined, language: string): string {
  if (typeof value === "string") return value;
  if (!value) return "";

  const normalizedEntries = Object.entries(value).map(([key, text]) => [normalizeLanguage(key), text] as const);
  const current = normalizeLanguage(language);
  const base = current.split("-")[0];
  for (const candidate of [current, base]) {
    const match = normalizedEntries.find(([key, text]) => key === candidate && text.trim() !== "");
    if (match) return match[1];
  }
  const regional = normalizedEntries.find(([key, text]) => key.split("-")[0] === base && text.trim() !== "");
  if (regional) return regional[1];
  const english = normalizedEntries.find(([key, text]) => key === "en" && text.trim() !== "");
  return (english ?? normalizedEntries.find(([, text]) => text.trim() !== ""))?.[1] ?? "";
}

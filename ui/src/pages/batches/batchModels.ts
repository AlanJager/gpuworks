/** The largest file whose requests the create dialog reads in the browser to list its models. */
export const FILE_MODEL_SCAN_MAX_BYTES = 50 * 1024 * 1024;

/** The distinct body.model values of a JSON Lines text, in first-seen order; lines that are not requests are the server's to report. */
export function modelsInJSONL(text: string): string[] {
  const seen = new Set<string>();
  for (const line of text.split("\n")) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    try {
      const model = (JSON.parse(trimmed) as { body?: { model?: unknown } } | null)?.body?.model;
      if (typeof model === "string" && model) seen.add(model);
    } catch { /* not a request line */ }
  }
  return Array.from(seen);
}

/**
 * modelsInFile lists the models the requests file names; null when the file is
 * over the scan limit or cannot be read, and the check is skipped.
 */
export async function modelsInFile(file: File): Promise<string[] | null> {
  if (file.size > FILE_MODEL_SCAN_MAX_BYTES) return null;
  try {
    return modelsInJSONL(await file.text());
  } catch {
    return null;
  }
}

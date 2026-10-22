/**
 * Human-readable engine names used by deployment controls.
 *
 * The engine name is also the wire/API identity. Keep it visible in option
 * labels so custom engines cannot silently collapse into the SGLang label.
 */
const knownEngineNames: Record<string, string> = {
  vllm: "vLLM",
  sglang: "SGLang",
  callable: "Callable",
};

export function engineDisplayName(name: string): string {
  const normalized = name.trim().toLowerCase();
  return knownEngineNames[normalized] || name;
}

export function engineOptionLabel(name: string): string {
  const display = engineDisplayName(name);
  const normalized = name.trim().toLowerCase();
  // Keep the canonical ID visible even for built-in engines. This is what
  // distinguishes similarly branded adapters (for example sglang/callable)
  // and makes the value easy to match with server logs and API payloads.
  return knownEngineNames[normalized] ? `${display} · ${name}` : display;
}

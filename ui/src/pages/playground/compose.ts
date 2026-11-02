import { localizedText } from "../../i18n/localizedText";
import type { InputField, InferenceAPIOperation } from "../../inferenceApi";

export function composeRequest(operation: InferenceAPIOperation, model: string, primary: Record<string, unknown>, values: Record<string, unknown>, raw?: string): Record<string, unknown> {
  const body: Record<string, unknown> = { ...primary, ...(operation.request_defaults || {}) };
  const declared = new Set((operation.inputs || []).map((field) => field.name));
  for (const field of operation.inputs || []) if (field.default !== undefined) body[field.name] = field.default;
  for (const [name, value] of Object.entries(values)) { if (!declared.has(name)) throw new Error(`Input ${name} is not declared by ${operation.type}`); body[name] = value; }
  for (const field of operation.inputs || []) if (field.required && body[field.name] === undefined) throw new Error(`Required input ${field.name} is missing`);
  if (raw?.trim()) { const expert = JSON.parse(raw) as unknown; if (!expert || Array.isArray(expert) || typeof expert !== "object") throw new Error("Expert request must be a JSON object"); Object.assign(body, expert); }
  body.model = model; return body;
}
export function inputLabel(field: InputField, language: string): string { return localizedText(field.label, language) || field.name; }
export function declaredValues(operation: InferenceAPIOperation, values: Record<string, unknown>): Record<string, unknown> {
  const declared = new Set((operation.inputs || []).map((field) => field.name));
  return Object.fromEntries(Object.entries(values).filter(([name]) => declared.has(name)));
}

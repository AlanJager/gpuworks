import type { DetectedTemplateChip, GPUPredicate, InferenceSpec, InferenceTemplate } from "../api";
import type { OperationType } from "../inferenceApi";
import { operationTypesOf } from "../modelInference";

export function predicateLabel(spec: InferenceSpec) {
  const parts = [spec.gpus.vendor.toUpperCase()];
  if (spec.gpus.cc) parts.push(`CC ${spec.gpus.cc}`);
  if (spec.gpus.name) parts.push(spec.gpus.name);
  return parts.join(" · ");
}

type Translate = (key: string, options?: Record<string, unknown>) => string;

function readablePredicateTarget(spec: InferenceSpec) {
  const configured = spec.gpus.display_target?.trim().toLowerCase();
  if (configured) return configured;
  const name = spec.gpus.name || "";
  const known: Array<[RegExp, string]> = [
    [/(?:DGX.*Spark|GB10)/i, "gb10"], [/(?:DGX.*Station|GB300)/i, "gb300"],
    [/RTX.*PRO.*6000/i, "rtx_pro_6000"], [/RTX.*PRO.*5000/i, "rtx_pro_5000"],
    [/RTX.*5090/i, "rtx_5090"], [/RTX.*4090/i, "rtx_4090"],
    [/GB300/i, "gb300"], [/GB200/i, "gb200"], [/B300/i, "b300"], [/B200/i, "b200"],
  ];
  return known.find(([pattern]) => pattern.test(name))?.[1] || spec.gpus.vendor.toLowerCase();
}

function defaultTargetName(key: string) {
  const names: Record<string, string> = {
    nvidia: "NVIDIA", ascend: "Ascend", hygon: "Hygon", amd: "AMD", ppu: "PPU", apple: "Apple silicon",
    turing: "Turing", hopper: "Hopper", blackwell: "Blackwell", gb10: "GB10",
    b200: "B200", b300: "B300", gb200: "GB200", gb300: "GB300",
    rtx_4090: "RTX 4090", rtx_5090: "RTX 5090", rtx_pro_5000: "RTX PRO 5000", rtx_pro_6000: "RTX PRO 6000",
  };
  return names[key] || key.split("_").join(" ").toUpperCase();
}

export function templateSpecDisplayName(spec: InferenceSpec, t: Translate) {
  const target = templateSpecTargetName(spec, t);
  const profile = spec.display_profile || "general";
  return t(`displayProfile.${profile}`, { target, defaultValue: `${target} Template` });
}

export function templateSpecTargetName(spec: InferenceSpec, t: Translate) {
  return gpuPredicateTargetName(spec.gpus, t);
}

export function gpuPredicateTargetName(predicate: GPUPredicate, t: Translate) {
  const targetKey = readablePredicateTarget({ gpus: predicate } as InferenceSpec);
  return t(`displayTarget.${targetKey}`, { defaultValue: defaultTargetName(targetKey) });
}

export function templateModelOperations(model: InferenceTemplate): OperationType[] {
  return operationTypesOf(model.specs.flatMap((spec) => spec.api?.operations ?? []));
}

/** @deprecated kept for consumers of the pre-operation catalog helper. */

function compactParameterCount(value?: string) {
  if (!value) return "";
  const scale = value.match(/(?:^|\s)(\d+(?:\.\d+)?\s*[BMKT])(?=\s|$|[·(),])/i)?.[1];
  if (scale) return scale.replace(/\s+/g, "").toUpperCase();
  return value.length <= 24 ? value : "";
}

function compactArchitecture(value?: string) {
  if (!value) return "";
  if (/mixture.of.experts|\bmoe\b|routed experts?/i.test(value)) return "moe";
  if (value.length <= 24 && !value.includes(";")) return value;
  if (/conditionalgeneration|vision|multimodal/i.test(value)) return "multimodal";
  if (/causallm/i.test(value)) return "causal LM";
  return "";
}

export function compactModelMetadata(model: InferenceTemplate) {
  return [compactParameterCount(model.parameter_count), compactArchitecture(model.architecture), model.context_length ? `${model.context_length.toLocaleString()} ctx` : ""].filter(Boolean).join(" · ");
}

export function chipKey(chip: DetectedTemplateChip): string {
  return [chip.vendor.trim().toLowerCase(), chip.name.trim(), chip.cc || ""].join("\u0000");
}

export function chipDisplayLabel(chip: Pick<DetectedTemplateChip, "vendor" | "name" | "cc">): string {
  const rawName = chip.name.trim();
  const shortName = rawName
    .replace(/^NVIDIA\s+/i, "")
    .replace(/^(?:GeForce|Quadro|Tesla)\s+/i, "")
    .replace(/^Ascend\s+/i, "")
    .replace(/^Hygon\s+/i, "")
    .replace(/^Apple\s+/i, "")
    .replace(/^vGPU[-_ ]*/i, "vGPU ")
    .replace(/\s+/g, " ")
    .trim();
  const fallbackName = shortName || chip.vendor.trim().toUpperCase() || "GPU";
  const hasConcreteModel = !/^(?:v?gpu(?:\s*[-_]?[0-9]+\s*(?:g|gb|b)?)?|graphics device|3d controller|unknown|generic)$/i.test(fallbackName);
  return hasConcreteModel || !chip.cc ? fallbackName : `${fallbackName} · CC ${chip.cc}`;
}

export function matchesComputeCapability(value: string | undefined, expression: string | undefined): boolean {
  if (!expression) return true;
  const cc = Number(value);
  if (!Number.isFinite(cc)) return false;
  if (expression.startsWith(">=")) return cc >= Number(expression.slice(2));
  if (expression.startsWith("<")) return cc < Number(expression.slice(1));
  if (expression.includes("..")) {
    const [lower, upper] = expression.split("..").map(Number);
    return cc >= lower && cc <= upper;
  }
  return cc === Number(expression);
}

export function chipMatchesPredicate(chip: DetectedTemplateChip, predicate: GPUPredicate): boolean {
  if (chip.vendor.trim().toLowerCase() !== predicate.vendor.trim().toLowerCase()) return false;
  if (!matchesComputeCapability(chip.cc, predicate.cc)) return false;
  if (!predicate.name) return true;
  try {
    return new RegExp(predicate.name, "i").test(chip.name);
  } catch {
    return false;
  }
}

export function modelMatchesChip(model: InferenceTemplate, chip?: DetectedTemplateChip): boolean {
  if (!chip) return true;
  return model.specs.some((spec) => chipMatchesPredicate(chip, spec.gpus) && !(model.unsupported || []).some((item) =>
    chipMatchesPredicate(chip, item.gpus) && (!item.quantization || item.quantization.toLowerCase() === (spec.quantization || "").toLowerCase())));
}

export function preferredTemplateEngine(specs: InferenceSpec[], chips: DetectedTemplateChip[]): string {
  const engines = [...new Set(specs.map((spec) => spec.engine))];
  const deployable = new Set(specs.filter((spec) => chips.some((chip) => chipMatchesPredicate(chip, spec.gpus))).map((spec) => spec.engine));
  return engines.sort((left, right) => Number(right === "vllm") - Number(left === "vllm")
    || Number(deployable.has(right)) - Number(deployable.has(left))
    || left.localeCompare(right))[0] || "";
}

export interface EditError { message: string; line?: number }

// Server YAML errors carry a line number and Go decoding noise; keep the
// reason and the line, drop the internals.
export function editErrorOf(raw: string): EditError {
  const line = /line (\d+)/i.exec(raw)?.[1];
  const cleaned = raw
    .replace(/^parse inference catalog YAML:\s*/i, "")
    .replace(/^yaml:\s*/i, "")
    .replace(/^unmarshal errors:\s*/i, "")
    .replace(/\s*in type [A-Za-z0-9_.]+\s*$/i, "")
    .trim();
  return { message: cleaned || raw, line: line ? Number(line) : undefined };
}

// The edited document's target identity: the first top-level model entry.
// Saving retargets the import onto this id, so a change from the loaded
// model_id is what the overwrite confirmation keys on. The server writes the
// models list indented, a hand-written document often does not, so the entry
// is found either right under the "models:" key at any indentation or as an
// unindented list item; whichever comes first in the text wins.
const yamlModelIDValue = String.raw`model_id:\s*(?:"([^"]+)"|'([^']+)'|(\S+))`;
const yamlModelIDPatterns = [
  new RegExp(String.raw`^models:[ \t]*\r?\n(?:[ \t]*(?:#.*)?\r?\n)*[ \t]*-[ \t]+${yamlModelIDValue}`, "m"),
  new RegExp(String.raw`^-[ \t]+${yamlModelIDValue}`, "m"),
];
export function yamlModelID(source: string): string | undefined {
  const matches = yamlModelIDPatterns.map((pattern) => pattern.exec(source)).filter((match): match is RegExpExecArray => match !== null);
  matches.sort((left, right) => left.index - right.index);
  return matches[0]?.slice(1).find(Boolean);
}

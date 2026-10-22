import type { TFunction } from "i18next";

export interface ModelOrganizationGroup {
  key: string;
  label: string;
  modelCount: number;
}

const modelOrganizationNames: Record<string, string> = {
  "ai-modelscope": "ModelScope",
  "arcee-ai": "Arcee AI",
  "baai": "BAAI",
  "baichuan-inc": "Baichuan",
  baidu: "Baidu",
  "bytedance-seed": "ByteDance",
  "deepseek-ai": "DeepSeek",
  "eco-tech": "Eco-Tech",
  "funaudiollm": "FunAudioLLM",
  "huggingfacetb": "Hugging Face",
  "ibm-granite": "IBM Granite",
  "inclusionai": "Inclusion AI",
  "lgai-exaone": "LG AI Research",
  "lmms-lab": "LMMs-Lab",
  "meituan-longcat": "Meituan LongCat",
  "meta-llama": "Meta Llama",
  "meta-models": "Meta",
  "minimaxai": "MiniMax",
  "moonshotai": "Moonshot AI",
  "openbmb": "OpenBMB",
  "opengvlab": "OpenGVLab",
  "openmoss-team": "OpenMOSS",
  "paddlepaddle": "PaddlePaddle",
  "sgl-npu": "SGLang NPU",
  "shanghai_ai_laboratory": "Shanghai AI Lab",
  "stepfun-ai": "StepFun",
  "thinkingmachines": "Thinking Machines",
  "vllm-ascend": "vLLM Ascend",
  "wan-ai": "Wan AI",
  "xiaomimimo": "Xiaomi MiMo",
  "zai-org": "Z.ai",
};

export const modelOrganizationIconOrganizations = [
  "allenai",
  "arcee-ai",
  "baai",
  "baichuan-inc",
  "baidu",
  "bigcode",
  "black-forest-labs",
  "bytedance-seed",
  "cohereforai",
  "deepseek-ai",
  "dots-studio",
  "eleutherai",
  "funaudiollm",
  "google",
  "huggingfacetb",
  "huihui-ai",
  "hygon",
  "ibm-granite",
  "iic",
  "inclusionai",
  "internlm",
  "jetbrains",
  "jinaai",
  "lgai-exaone",
  "lightricks",
  "lightseekorg",
  "liquidai",
  "lmms-lab",
  "meituan-longcat",
  "meta-llama",
  "meta-models",
  "microsoft",
  "mindlab-research",
  "minimaxai",
  "mistralai",
  "moonshotai",
  "nvidia",
  "openai",
  "openbmb",
  "opengvlab",
  "openmoss-team",
  "orionstarai",
  "paddlepaddle",
  "pfnet",
  "poolside",
  "qwen",
  "sgl-npu",
  "shanghai_ai_laboratory",
  "skywork",
  "stabilityai",
  "stepfun-ai",
  "tencent",
  "thinkingmachines",
  "tongyi-mai",
  "upstage",
  "vllm-ascend",
  "wan-ai",
  "xiaomimimo",
  "xverse",
  "zai-org",
] as const;

const modelOrganizationIconSet = new Set<string>(modelOrganizationIconOrganizations);

export function modelOrganizationKey(modelID: string) {
  const separator = modelID.indexOf("/");
  return separator > 0 ? modelID.slice(0, separator).trim().toLowerCase() : "other";
}

export function modelOrganizationName(organization: string, t: TFunction<"inferenceTemplates">) {
  const key = organization.trim().toLowerCase();
  if (key === "other") return t("organization.other");
  return modelOrganizationNames[key] || key.split(/[-_]+/).filter(Boolean)
    .map((part) => part.length <= 3 ? part.toUpperCase() : `${part[0].toUpperCase()}${part.slice(1)}`)
    .join(" ");
}

export function modelOrganizationGroups(items: Array<{ model_id: string }>, t: TFunction<"inferenceTemplates">): ModelOrganizationGroup[] {
  const models = new Map<string, Set<string>>();
  for (const item of items) {
    const key = modelOrganizationKey(item.model_id);
    if (!key) continue;
    const group = models.get(key) || new Set<string>();
    group.add(item.model_id.toLowerCase());
    models.set(key, group);
  }
  return [...models.entries()].map(([key, modelIDs]) => ({
    key,
    label: modelOrganizationName(key, t),
    modelCount: modelIDs.size,
  })).sort((left, right) => left.label.localeCompare(right.label, "en", { sensitivity: "base" }));
}

export function modelOrganizationIcon(modelID: string) {
  const organization = modelOrganizationKey(modelID);
  return modelOrganizationIconSet.has(organization) ? `/model-org-icons/${organization}.webp` : undefined;
}

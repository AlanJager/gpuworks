import type { CompletionMessage, PlaygroundModel } from "./types";

export function supportsChatInput(model: PlaygroundModel | undefined, modality: string): boolean {
  // Legacy/external models with no declaration retain compatibility. An
  // explicit declaration is authoritative; output modalities are irrelevant.
  return !!model && (!model.input_modalities?.length || model.input_modalities.includes(modality));
}

export function unsupportedChatInput(model: PlaygroundModel | undefined, messages: CompletionMessage[]): string | undefined {
  for (const message of messages) {
    if (!Array.isArray(message.content)) continue;
    for (const part of message.content) {
      const modality = part?.type === "input_audio" || part?.type === "audio_url" ? "audio"
        : part?.type === "image_url" ? "image" : part?.type === "video_url" ? "video" : undefined;
      if (modality && !supportsChatInput(model, modality)) return modality;
    }
  }
}

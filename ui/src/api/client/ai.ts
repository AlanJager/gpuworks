import { readSSE } from "../../sse";
import { getJSON, localizedResponseError, sendJSON, type List } from "../http";
import type { AIAnswer, AIAskRequest, AIAvailability, AIConfig, AIConfigDeleteResult, AIConfigInput, AIConfigPatch, AIConfigTest, AIConfigTestResult, AIFinding, AIModelFit, AIPresetDictionary, AIStreamEvent } from "../ai";

export const aiApi = {
  aiAvailable: () => getJSON<AIAvailability>("/api/v1/ai/available"),
  aiConfigs: () =>
    getJSON<List<AIConfig>>("/api/v1/ai/configs"),
  aiPresets: () =>
    getJSON<AIPresetDictionary>("/api/v1/ai/presets"),
  createAIConfig: (body: AIConfigInput) =>
    sendJSON<AIConfig>("POST", "/api/v1/ai/configs", body),
  updateAIConfig: (id: number, body: AIConfigPatch) =>
    sendJSON<AIConfig>("PATCH", `/api/v1/ai/configs/${id}`, body),
  testAIConfig: (body: AIConfigTest) =>
    sendJSON<AIConfigTestResult>("POST", "/api/v1/ai/configs/test", body),
  deleteAIConfig: (id: number) =>
    sendJSON<AIConfigDeleteResult>("DELETE", `/api/v1/ai/configs/${id}`),
  aiAsk: (body: AIAskRequest, signal?: AbortSignal) =>
    sendJSON<AIAnswer>("POST", "/api/v1/ai/ask", body, signal),
  /**
   * aiAskStream asks with the answer streamed: onEvent sees each tool call,
   * text and reasoning delta, then one done or error event. A failure before
   * the model is asked (no endpoint, unknown subject) rejects like aiAsk.
   */
  aiAskStream: async (body: AIAskRequest, onEvent: (event: AIStreamEvent) => void, signal?: AbortSignal) => {
    const path = "/api/v1/ai/ask";
    const res = await fetch(path, {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json", Accept: "text/event-stream" },
      body: JSON.stringify(body),
      signal,
    });
    if (!res.ok) throw await localizedResponseError(res, `${path}: ${res.status}`);
    if (!res.body) throw new Error(`${path}: no response body`);
    await readSSE(res.body, (type, data) => {
      if (!["step", "text", "reasoning", "done", "error"].includes(type)) return;
      onEvent({ type, ...JSON.parse(data) } as AIStreamEvent);
    });
  },
  aiInstanceFindings: (uuid: string, signal?: AbortSignal) =>
    getJSON<List<AIFinding>>(`/api/v1/ai/instances/${encodeURIComponent(uuid)}/findings`, signal),
  aiModelFit: (id: number, signal?: AbortSignal) =>
    getJSON<AIModelFit>(`/api/v1/ai/models/${id}/fit`, signal),
};

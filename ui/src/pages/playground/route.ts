import type { InferenceAPIOperation } from "../../inferenceApi";

export type PlaygroundRouteOperation = Pick<InferenceAPIOperation, "type" | "renderer">;

export function playgroundRoute(operation: PlaygroundRouteOperation, model?: string, selection?: Record<string, string>): string {
  const mode = operation.renderer === "rerank" ? "vector" : operation.renderer;
  const query = new URLSearchParams({ ...selection, operation: operation.type });
  if (model) query.set("model", model);
  return `/playground/${mode}?${query}`;
}

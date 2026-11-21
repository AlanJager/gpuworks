import { sendJSON } from "../http";
import type { EvaluateResponse } from "../evaluate";

export const evaluateApi = {
  evaluate: (body: unknown, signal?: AbortSignal) =>
    sendJSON<EvaluateResponse>("POST", "/api/v1/evaluate", body, signal),
};

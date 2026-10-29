import type { BatchEndpoint } from "../../batchApi";

// A sample's request bodies by endpoint: the smallest body the endpoint
// accepts, so a first batch shows the file shape and runs anywhere.
const sampleBodies: Record<BatchEndpoint, (model: string, n: number) => Record<string, unknown>> = {
  "/v1/chat/completions": (model, n) => ({
    model,
    messages: [{ role: "user", content: sampleQuestions[n % sampleQuestions.length] }],
    max_tokens: 128,
  }),
  "/v1/completions": (model, n) => ({
    model,
    prompt: sampleQuestions[n % sampleQuestions.length],
    max_tokens: 128,
  }),
  "/v1/embeddings": (model, n) => ({
    model,
    input: sampleQuestions[n % sampleQuestions.length],
  }),
};

const sampleQuestions = [
  // i18n-ignore: sample request content sent to the model, not UI text
  "用一句话解释什么是 GPU。",
  "Write a haiku about autumn.",
  // i18n-ignore: sample request content sent to the model, not UI text
  "列出三种常见的排序算法。",
  "What is the capital of France?",
  // i18n-ignore: sample request content sent to the model, not UI text
  "把“你好，世界”翻译成英文。",
];

// SAMPLE_LINES is how many requests a sample file holds.
export const SAMPLE_LINES = 5;

/**
 * batchSampleJSONL is a ready-to-upload requests file for the endpoint and
 * model: one request per line with a unique custom_id, method POST, the
 * endpoint as url and a body naming the model.
 */
export function batchSampleJSONL(endpoint: BatchEndpoint, model: string, lines = SAMPLE_LINES): string {
  const out: string[] = [];
  for (let n = 0; n < lines; n++) {
    out.push(JSON.stringify({
      custom_id: `request-${n + 1}`,
      method: "POST",
      url: endpoint,
      body: sampleBodies[endpoint](model, n),
    }));
  }
  return out.join("\n") + "\n";
}

// sampleFileName names the downloaded sample after its endpoint.
export function sampleFileName(endpoint: BatchEndpoint): string {
  return `batch-sample-${endpoint.replace(/^\/v1\//, "").replace(/\//g, "-")}.jsonl`;
}

/**
 * downloadBatchSample hands the browser the sample as a file to save.
 */
export function downloadBatchSample(endpoint: BatchEndpoint, model: string): void {
  const blob = new Blob([batchSampleJSONL(endpoint, model)], { type: "application/jsonl" });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = sampleFileName(endpoint);
  document.body.appendChild(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

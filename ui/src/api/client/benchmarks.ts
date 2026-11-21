import { getJSON, localizedResponseError, sendJSON, type List } from "../http";
import type { BenchmarkCurveResponse, BenchmarkDatasetProfile, BenchmarkMeta, BenchmarkRequestTimeline, BenchmarkRun } from "../benchmarks";

export const benchmarksApi = {
  benchmarks: () => getJSON<List<BenchmarkRun>>("/api/v1/benchmarks"),
  benchmarkMeta: () => getJSON<BenchmarkMeta>("/api/v1/benchmarks/meta"),
  benchmark: (id: number) => getJSON<BenchmarkRun>(`/api/v1/benchmarks/${id}`),
  benchmarkCurve: (id: number) =>
    getJSON<BenchmarkCurveResponse>(`/api/v1/benchmarks/${id}/curve`),
  benchmarkPointTimeline: (id: number, seq: number, signal?: AbortSignal) =>
    getJSON<BenchmarkRequestTimeline>(`/api/v1/benchmarks/${id}/points/${seq}/timeline`, signal),
  createBenchmark: (body: unknown) =>
    sendJSON<BenchmarkRun>("POST", "/api/v1/benchmarks", body),
  benchmarkModels: (body: unknown) =>
    sendJSON<{ models: string[]; normalized_url: string; notice?: string }>(
      "POST",
      "/api/v1/benchmarks/models",
      body,
    ),
  importBenchmarkDataset: async (file: File) => {
    const res = await fetch("/api/v1/benchmarks/datasets/import", {
      method: "POST",
      credentials: "include",
      body: file,
    });
    if (!res.ok)
      throw await localizedResponseError(res, `dataset import: ${res.status}`);
    return (await res.json()) as {
      kind: "imported";
      profile?: BenchmarkDatasetProfile;
      samples: Array<{
        messages: Array<{ role: string; content: string }>;
        input_tokens?: number;
        output_tokens?: number;
        wait_millis?: number;
      }>;
    };
  },
  importBenchmarkTokenizer: async (file: File) => {
    const res = await fetch("/api/v1/benchmarks/tokenizers/import", {
      method: "POST",
      credentials: "include",
      body: file,
    });
    if (!res.ok)
      throw await localizedResponseError(
        res,
        `tokenizer import: ${res.status}`,
      );
    return (await res.json()) as { tokenizer_ref: string; name: string };
  },
  cancelBenchmark: (id: number) =>
    sendJSON<BenchmarkRun>("POST", `/api/v1/benchmarks/${id}/cancel`),
  deleteBenchmark: (id: number) =>
    sendJSON<void>("DELETE", `/api/v1/benchmarks/${id}`),
};

import assert from "node:assert/strict";
import test from "node:test";

import { benchmarkErrorText } from "../src/pages/evaluations/evaluationUtils.ts";

test("known benchmark persisted error codes translate and historical text remains unchanged", () => {
  const t = ((key: string) => key === "benchmarkError.BENCHMARK_SERVER_RESTARTED" ? "服务重启导致评测中断" : key) as never;
  assert.equal(benchmarkErrorText(t, "BENCHMARK_SERVER_RESTARTED"), "服务重启导致评测中断");
  assert.equal(benchmarkErrorText(t, "legacy raw error"), "legacy raw error");
});

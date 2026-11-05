import assert from "node:assert/strict";
import { test } from "node:test";
import { formatGPUCapacity, gpuCapacityBytes } from "../src/gpuMemory.ts";
import { fmtGiB } from "../src/pages/gpus/gpuFormat.ts";

test("inventory capacity follows the API fact, including absent capacity and measured zero usage", () => {
  assert.equal(formatGPUCapacity({ memory_total_bytes: 0, memory_capacity_known: false }, fmtGiB), "—");
  assert.equal(formatGPUCapacity({ memory_total_bytes: 96 * 1024 ** 3, memory_capacity_known: false }, fmtGiB), "—");
  for (const gib of [24, 64, 96]) {
    assert.equal(formatGPUCapacity({ memory_total_bytes: gib * 1024 ** 3, memory_capacity_known: true }, fmtGiB), `${gib} GiB`);
  }
  assert.equal(fmtGiB(0), "0.0 GiB");
});

test("older API responses use one capacity compatibility boundary", () => {
  assert.equal(gpuCapacityBytes({ memory_total_bytes: 0 }), undefined);
  assert.equal(gpuCapacityBytes({ memory_total_bytes: 24 }), 24);
});

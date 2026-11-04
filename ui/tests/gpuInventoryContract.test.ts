import assert from "node:assert/strict";
import fs from "node:fs";
import { test } from "node:test";
import type { PhysicalGPU } from "../src/api.ts";
import { formatGPUCapacity } from "../src/gpuMemory.ts";
import { fmtGiB } from "../src/pages/gpus/gpuFormat.ts";

// Captured from the real GPU inventory handler, including a card whose
// capacity has not been reported. This must render as unknown, not zero GiB.
const inventory = JSON.parse(fs.readFileSync(
  new URL("./fixtures/gpu-inventory/gpus-page.json", import.meta.url), "utf8",
)) as { items: PhysicalGPU[] };

test("the inventory wire distinguishes measured capacity from an unknown card", () => {
  const known = inventory.items.find((gpu) => gpu.name === "Quadro RTX 8000");
  const unknown = inventory.items.find((gpu) => gpu.name === "GeForce RTX 3090");
  assert.equal(known !== undefined && unknown !== undefined, true);
  assert.equal(known!.memory_capacity_known, true);
  assert.equal(formatGPUCapacity(known!, fmtGiB), "45 GiB");
  assert.equal(unknown!.memory_capacity_known, false);
  assert.equal(formatGPUCapacity(unknown!, fmtGiB), "—");
});

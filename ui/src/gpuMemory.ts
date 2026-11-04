type GPUCapacity = { memory_total_bytes: number; memory_capacity_known?: boolean };

// Current servers declare whether the inventory measurement is known. Keep
// the old API's zero sentinel in one compatibility boundary, not in views.
export function gpuCapacityBytes(gpu: GPUCapacity): number | undefined {
  const known = gpu.memory_capacity_known ?? (gpu.memory_total_bytes > 0);
  return known ? gpu.memory_total_bytes : undefined;
}

export function formatGPUCapacity(gpu: GPUCapacity, format: (bytes: number) => string): string {
  const bytes = gpuCapacityBytes(gpu);
  return bytes === undefined ? "—" : format(bytes);
}

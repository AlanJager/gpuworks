import type { Node, PartitionCapability } from "../api";

export function capabilityAllowsPartition(capability?: PartitionCapability | null): boolean {
  return capability?.status === "ready";
}

export function domesticPartitionCapability(node?: Node | null): PartitionCapability | undefined {
  return node?.partition_capabilities?.find((capability) => capability.vendor !== "nvidia");
}

export function readyPartitionCapability(node?: Node | null): PartitionCapability | undefined {
  const capabilities = node?.partition_capabilities ?? [];
  return capabilities.find((capability) => capability.vendor !== "nvidia" && capabilityAllowsPartition(capability))
    ?? capabilities.find((capability) => capabilityAllowsPartition(capability));
}

export function nodeSupportsPartition(node?: Node | null): boolean {
  return Boolean(readyPartitionCapability(node));
}

// The only selected-label match is after the first five cluster refusals.
export const nodes = Array.from({ length: 14 }, (_, index) => ({
  uuid: `node-${index}`, name: index === 13 ? "selected-turing" : `other-${index}`,
  hostname: index === 13 ? "selected-turing" : `other-${index}`, state: "online",
  gpu_count: 1, labels: { runtime_type: index === 13 ? "sandbox" : "docker" },
  system_labels: [{ key: "gpuworks.ai/vendor", value: "nvidia", display: "NVIDIA" }],
  gpus: [{ index: 0, name: "RTX 2080 Ti", vendor: "nvidia", compute_capability: "7.5" }],
}));
const chipReason = { code: "chip_mismatch", params: {
  predicate: '{"vendor":"nvidia","cc":">=8.0"}', required_vendor: "nvidia", required_cc: ">=8.0", required_name: "",
  actual_gpus: [{ name: "RTX 2080 Ti", vendor: "nvidia", cc: "7.5" }],
} };
export const evaluation = {
  ok: true, effective_params: {}, warnings: [], template_deviated: false, argv: [], image: "fixture",
  eligible_nodes: nodes.map((node, index) => ({ uuid: node.uuid, name: node.name, eligible: false, requirements_match: index === 13,
    reasons: [index === 13 ? chipReason : { code: "node_requirements_unmet", params: { detail: "runtime_type" } }],
  })),
  candidates: nodes.map((node, index) => ({ node_uuid: node.uuid, node_name: node.name, ok: false,
    reason: index === 13 ? chipReason : { code: "node_requirements_unmet", params: { detail: "runtime_type" } },
  })),
};

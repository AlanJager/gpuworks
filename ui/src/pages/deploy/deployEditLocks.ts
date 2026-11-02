// The request fields an edit cannot change (the draft's `locked` list, the
// server's judgement for the deployment's shape) and the deploy form controls
// that state each of them. The list of locked fields is never written here;
// this table only says where each one is drawn.
const controlsOfField: Record<string, string[]> = {
  model_id: ["model_id"],
  template_id: ["template"],
  template_spec_source: ["template"],
  template_spec_name: ["template"],
  scenario: ["template"],
  engine: ["engine"],
  target_vendor: ["target_vendor"],
  name: ["name"],
  served_name: ["served_name"],
  model_source: ["model_source"],
  nnodes: ["topology"],
  node_uuid: ["node"],
  node_uuids: ["nodes"],
  node_requirements: ["node_requirements"],
  placement_mode: ["placement_mode"],
  placement_strategy: ["placement_strategy"],
  master_addr: ["rank_address"],
  master_port: ["rank_address"],
  dist_group: ["rank_address"],
  dist_role: ["rank_address"],
  node_rank: ["rank_address"],
  replicas: ["replicas"],
  vram_limit_gib: ["vram_limit"],
  partition_backend: ["vram_limit"],
  placement: ["pd_placement"],
};

/** The form controls a set of locked request fields locks. */
export function lockedControls(lockedFields: readonly string[] = []): Set<string> {
  return new Set(lockedFields.flatMap((field) => controlsOfField[field] ?? []));
}

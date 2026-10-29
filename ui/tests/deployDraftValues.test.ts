import assert from "node:assert/strict";
import { test } from "node:test";

import type { EngineProfile } from "../src/api.ts";
import {
  buildPayload,
  deploymentQuery,
  draftToFormValues,
} from "../src/pages/deploy/deployHelpers.ts";
import { cloneIdentity, templateSpecKey } from "../src/pages/deploy/deployDraftValues.ts";
import {
  changedFormValues,
  editPayload,
  payloadChanges,
} from "../src/pages/deploy/deployEditPayload.ts";
import { lockedControls } from "../src/pages/deploy/deployEditLocks.ts";
import { engineProfiles } from "./helpers/engineFixture.ts";

const vllm = engineProfiles.find((profile) => profile.name === "vllm") as EngineProfile;

// The locked lists the server sends (internal/server/api/deployments_draft.go).
const identity = [
  "model_id", "template_id", "template_spec_source", "template_spec_name", "scenario",
  "engine", "target_vendor", "name", "served_name", "model_source", "nnodes",
];
const perRank = [
  "node_uuid", "node_uuids", "node_requirements", "placement_mode", "placement_strategy",
  "master_addr", "master_port", "dist_group", "dist_role", "node_rank",
  "replicas", "vram_limit_gib", "partition_backend",
];

const manualDraft = {
  model_id: 3, name: "qwen", served_name: "qwen-served", engine: "vllm",
  image: "example/vllm:test", target_vendor: "nvidia", placement_mode: "manual",
  node_uuid: "node-1", replicas: 2, routing_policy: "round_robin", nnodes: 1,
  extra_args: "--served-model-name x", sleepable: true, health_check: false,
  health_check_threshold_sec: 90, tokenize_enabled: false, start_timeout_sec: 900,
  placement_strategy: "binpack",
  quick_params: { max_model_len: 8192, gpu_memory_utilization: 0.85, enable_prefix_caching: false },
  placement: { tp: 2, pp: 1, gpus: [0, 1] },
  vram_limit_gib: 16, partition_backend: "hami-core",
};

// What a draft states must come back from the form unchanged. The payload is
// the editable fields of the request, so each is compared as the server reads it.
function assertSameFields(payload: Record<string, unknown>, request: Record<string, unknown>, fields: string[]) {
  for (const field of fields) assert.deepEqual(payload[field], request[field], field);
}

test("a manual single-node draft round-trips through the form into the same request", () => {
  const values = draftToFormValues(manualDraft, vllm);
  assert.equal(values.node_selection_mode, "manual");
  assert.equal(values.image_mode, "custom", "the stored image is submitted as it is");
  const payload = editPayload(values, [], vllm, [], { request: manualDraft, locked: identity });
  assert.ok(payload);
  assertSameFields(payload, manualDraft, [
    "model_id", "name", "served_name", "engine", "image", "target_vendor", "placement_mode",
    "node_uuid", "replicas", "extra_args", "health_check", "health_check_threshold_sec",
    "tokenize_enabled", "start_timeout_sec", "placement_strategy", "placement", "nnodes", "routing_policy",
  ]);
  assert.equal(payload.sleepable, true);
  assert.deepEqual(payload.quick_params, manualDraft.quick_params);
  assert.equal(payload.vram_limit_gib, 16);
  assert.equal(payload.partition_backend, "hami-core");
});

test("a dictionary key the deployment does not state is removed explicitly, not defaulted", () => {
  const values = draftToFormValues(manualDraft, vllm);
  assert.equal(values.max_num_seqs, "");
  assert.equal(values.max_model_len, 8192);
  const payload = editPayload(values, [], vllm, [], { request: manualDraft, locked: identity }) as Record<string, unknown>;
  const unset = payload.quick_params_unset as string[];
  assert.ok(unset.includes("max_num_seqs"));
  for (const key of Object.keys(manualDraft.quick_params)) assert.ok(!unset.includes(key), key);
  // without the profile nothing is guessed
  assert.equal(draftToFormValues(manualDraft).max_num_seqs, undefined);
});

test("a stored key the form stopped stating travels as an unset", () => {
  const values = draftToFormValues(manualDraft, vllm);
  values.max_model_len = undefined; // the deployer chose "automatic"
  const payload = editPayload(values, [], vllm, [], { request: manualDraft, locked: identity }) as Record<string, unknown>;
  assert.ok(!("max_model_len" in (payload.quick_params as object)));
  assert.ok((payload.quick_params_unset as string[]).includes("max_model_len"));
});

test("an automatic draft with a pool and labels states requirements, not the previewed node", () => {
  const request = {
    model_id: 3, name: "auto", served_name: "auto", engine: "vllm", target_vendor: "nvidia",
    placement_mode: "auto", node_uuid: "preview-node", replicas: 1, nnodes: 1,
    placement_strategy: "spread",
    node_requirements: {
      "gpuworks.ai/node": ["n1", "n2"], "gpuworks.ai/rdma": ["true"], zone: ["east"], tier: ["a", "b"],
    },
    quick_params: { max_model_len: 4096 }, placement: { tp: 2, gpus: [0, 1] },
  };
  const values = draftToFormValues(request, vllm);
  assert.equal(values.node_selection_mode, "auto");
  assert.equal(values.node_uuid, undefined);
  assert.deepEqual(values.pool_nodes, ["n1", "n2"]);
  assert.deepEqual(values.node_labels, { zone: "east" });
  assert.deepEqual(values.system_conditions, ["gpuworks.ai/rdma=true", "tier=a", "tier=b"]);
  const payload = editPayload(values, [], vllm, [], { request, locked: identity }) as Record<string, unknown>;
  assert.equal(payload.placement_mode, "auto");
  assert.ok(!("node_uuid" in payload));
  assert.deepEqual(payload.node_requirements, request.node_requirements);
  assert.deepEqual(payload.placement, { tp: 2 }, "the scheduler picks the GPUs again");
});

test("container overrides round-trip: env, docker args, shm, mounts, command", () => {
  const overrides = {
    model_mount_path: "/weights", env: { HF_TOKEN: "x", NCCL_DEBUG: "INFO" }, env_unset: ["OLD"],
    docker_args: ["--ipc=host"], shm_bytes: 8 * 2 ** 30,
    extra_mounts: [{ host_path: "/srv/data", container_path: "/data", read_only: true, purpose: "data" }],
    entrypoint_display: "python", cmd_display: "serve.py --x", command_mode: "manual",
    command_includes_preparation: true,
  };
  const request = { ...manualDraft, container_overrides: overrides };
  const values = draftToFormValues(request, vllm);
  assert.deepEqual(values.container_env_rows, [
    { key: "HF_TOKEN", value: "x" }, { key: "NCCL_DEBUG", value: "INFO" },
  ]);
  assert.equal(values.container_shm_gib, 8);
  const { payload } = buildPayload(values, vllm);
  assert.deepEqual(payload.container_overrides, overrides);
});

test("a draft without overrides or image states neither", () => {
  const request = { ...manualDraft, image: "", container_overrides: undefined };
  const values = draftToFormValues(request, vllm);
  assert.equal(values.image_mode, "auto");
  const payload = editPayload(values, [], vllm, [], { request, locked: identity }) as Record<string, unknown>;
  assert.ok(!("image" in payload));
  assert.ok(!("container_overrides" in payload));
});

test("a multi-node draft round-trips with every per-rank field exactly as stored", () => {
  const request = {
    model_id: 3, name: "multi", served_name: "multi", engine: "vllm", target_vendor: "nvidia",
    image: "example/vllm:test", nnodes: 2, node_uuid: "a", node_uuids: ["a", "b"], replicas: 1,
    master_addr: "10.0.0.1", master_port: 29500, dist_group: "grp", dist_role: "leader",
    placement_strategy: "spread", quick_params: { max_model_len: 8192 },
    placement: { tp: 1, pp: 2, units: [{ node_uuid: "a", followers: [{ node_uuid: "b" }] }] },
  };
  const values = draftToFormValues(request, vllm);
  assert.equal(values.node_selection_mode, "multi");
  assert.deepEqual(values.distributed_nodes, ["a", "b"]);
  const locked = [...identity, ...perRank];
  const payload = editPayload(values, [], vllm, [], { request, locked }) as Record<string, unknown>;
  assertSameFields(payload, request, [
    "nnodes", "node_uuid", "node_uuids", "replicas", "master_addr", "master_port", "dist_group", "dist_role", "placement",
  ]);
  assert.ok(!("placement_mode" in payload), "a distributed deployment has no placement mode");
  assert.equal(payload.placement_strategy, "spread", "a locked field travels as the draft states it");
});

test("a PD draft keeps its placement exactly and shows the PD topology", () => {
  const pd = {
    mode: "nixl", fallback: "none", kv_load_failure: "fail",
    roles: {
      decode: { tp: 1, units: [{ node_uuid: "d1", gpus: [0] }] },
      prefill: { tp: 2, units: [{ node_uuid: "p1" }, { node_uuid: "p2" }] },
    },
  };
  const request = {
    model_id: 3, name: "pd", served_name: "pd", engine: "vllm", target_vendor: "nvidia", nnodes: 1,
    replicas: 1, quick_params: {}, placement: { pd },
  };
  const values = draftToFormValues(request, vllm);
  assert.equal(values.node_selection_mode, "pd");
  assert.equal(values.pd_mode, "nixl");
  const payload = editPayload(values, [], vllm, [], {
    request, locked: [...identity, ...perRank, "placement"],
  }) as Record<string, unknown>;
  assert.deepEqual(payload.placement, request.placement);
  assert.ok(!("placement_mode" in payload));
});

test("changes are counted per field, where they were made", () => {
  const base = { request: manualDraft, locked: identity };
  const was = editPayload(draftToFormValues(manualDraft, vllm), [], vllm, [], base);
  assert.deepEqual(payloadChanges(was, was), []);
  const values = draftToFormValues(manualDraft, vllm);
  values.max_model_len = 16384;
  values.max_num_seqs = 64;
  values.container_env_rows = [{ key: "A", value: "1" }];
  values.replicas = 3;
  const now = editPayload(values, [], vllm, [], base);
  assert.deepEqual(payloadChanges(was, now), [
    "container_overrides.env.A", "quick_params.max_model_len", "quick_params.max_num_seqs", "replicas",
  ]);
  assert.deepEqual(payloadChanges(null, now), []);
});

test("the deployer's edits are what differs from the draft they started from", () => {
  const baseline = draftToFormValues(manualDraft, vllm);
  const current = { ...baseline, max_model_len: 1024, max_num_seqs: undefined, replicas: baseline.replicas };
  assert.deepEqual(changedFormValues(current, baseline), { max_model_len: 1024, max_num_seqs: undefined });
});

test("the locked request fields map to the controls the form draws", () => {
  const controls = lockedControls(identity);
  for (const control of ["model_id", "template", "engine", "target_vendor", "name", "served_name", "model_source", "topology"])
    assert.ok(controls.has(control), control);
  assert.ok(!controls.has("replicas"));
  assert.ok(lockedControls(perRank).has("replicas"));
  assert.equal(lockedControls(undefined).size, 0);
  assert.equal(lockedControls(["something_new"]).size, 0, "an unknown field locks nothing here");
});

test("deploy links name the deployment to edit or clone, and the fields to show", () => {
  assert.deepEqual(
    (({ queryEditID, queryFromID, queryFocus }) => ({ queryEditID, queryFromID, queryFocus }))(
      deploymentQuery("?edit=12&focus=max_model_len, extra_args"),
    ),
    { queryEditID: 12, queryFromID: undefined, queryFocus: ["max_model_len", "extra_args"] },
  );
  assert.equal(deploymentQuery("?from=7").queryFromID, 7);
  assert.equal(deploymentQuery("?edit=3&from=7").queryFromID, undefined, "edit wins");
  assert.equal(deploymentQuery("?edit=abc").queryEditID, undefined);
  assert.equal(deploymentQuery("?edit=-1").queryEditID, undefined);
  assert.deepEqual(deploymentQuery("").queryFocus, []);
});

test("a clone gets suffixed names and a draft names its template spec", () => {
  const values = cloneIdentity({ name: "qwen", served_name: "", other: 1 });
  assert.deepEqual(values, { name: "qwen-clone", served_name: "", other: 1 });
  assert.equal(templateSpecKey({ template_id: 4, template_spec_source: "builtin", template_spec_name: "a" }), "4:builtin:a");
  assert.equal(templateSpecKey({ model_id: 1 }), undefined);
});

test("a cloned draft keeps its yield preset, and an edit never states a yield policy", () => {
  const leveled = { ...manualDraft, yield_policy: { preset: "online" } };
  const values = draftToFormValues(leveled, vllm);
  assert.equal(values.yield_preset, "online");
  // A stored policy read whole carries the preset the server derived.
  assert.equal(draftToFormValues({ ...manualDraft, yield_policy: { preset: "shared", policy: "timeslice", auto_sleep: false } }, vllm).yield_preset, "shared");
  // Custom has no place in the form's preset select, and an absent policy is resident.
  assert.equal(draftToFormValues({ ...manualDraft, yield_policy: { preset: "custom", policy: "never", auto_sleep: true } }, vllm).yield_preset, undefined);
  assert.equal(draftToFormValues(manualDraft, vllm).yield_preset, undefined);
  // The policy is changed on the scheduling policy card; the server ignores it on an edit.
  const payload = editPayload({ ...values, yield_preset: "offline" }, [], vllm, [], { request: leveled, locked: identity });
  assert.ok(payload);
  assert.equal("yield_policy" in payload, false);
  const plain = editPayload({ ...values, yield_preset: "offline" }, [], vllm, [], { request: manualDraft, locked: identity });
  assert.ok(plain);
  assert.equal("yield_policy" in plain, false);
});

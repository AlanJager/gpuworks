import assert from "node:assert/strict";
import test from "node:test";

import { buildPayload, deployDraftPayload } from "../src/pages/deploy/deployHelpers.ts";
import { draftToFormValues } from "../src/pages/deploy/deployDraftValues.ts";
import { buildPdPlacement, unitsPlacementAuto } from "../src/pages/deploy/deployPd.ts";

// A multi-node or PD draft is placed by the platform unless the deployer
// chose to name its nodes: the payload then states placement_mode "auto", no
// node, no card, and keeps the sizes (TP, PP, units per role) the deployer set.

const pdRoles = {
  decode: { tp: 2, units: [{ node_uuid: "n-1", gpus: [0, 1] }, {}] },
  prefill: { tp: 1, pp: 2, units: [{ node_uuid: "n-2", followers: [{ node_uuid: "n-3" }] }] },
};
const pdDraft = (extra: Record<string, unknown> = {}) => ({
  model_id: 3, node_selection_mode: "pd", nnodes: 1, pd_mode: "vllm-nixl", pd_fallback: "decode", pd_roles: pdRoles, ...extra,
});
const multiDraft = (extra: Record<string, unknown> = {}) => ({
  model_id: 3, node_selection_mode: "multi", nnodes: 2, tp: 8, distributed_nodes: ["n-1", "n-2"], node_uuid: "n-1", master_addr: "10.0.0.1", master_port: 29500, ...extra,
});

test("multi-node and PD drafts are automatic until the deployer says manual", () => {
  assert.equal(unitsPlacementAuto(pdDraft()), true);
  assert.equal(unitsPlacementAuto(pdDraft({ units_placement: "auto" })), true);
  assert.equal(unitsPlacementAuto(pdDraft({ units_placement: "manual" })), false);
  assert.equal(unitsPlacementAuto(multiDraft()), true);
  assert.equal(unitsPlacementAuto(multiDraft({ units_placement: "manual" })), false);
  // A single node has its own automatic / manual position.
  assert.equal(unitsPlacementAuto({ node_selection_mode: "auto", nnodes: 1 }), false);
  assert.equal(unitsPlacementAuto({ node_selection_mode: "manual", nnodes: 1, units_placement: "auto" }), false);
});

test("an automatic PD draft states its units without node, cards or followers, keeping counts, TP and PP", () => {
  const pd = buildPdPlacement(pdDraft());
  assert.deepEqual(pd, {
    mode: "vllm-nixl",
    roles: {
      decode: { tp: 2, units: [{}, {}] },
      prefill: { tp: 1, pp: 2, units: [{}] },
    },
    fallback: "decode",
  });
  const manual = buildPdPlacement(pdDraft({ units_placement: "manual" })) as { roles: Record<string, { units: unknown[] }> };
  assert.deepEqual(manual.roles.decode.units, [{ node_uuid: "n-1", gpus: [0, 1] }, {}]);
  assert.deepEqual(manual.roles.prefill.units, [{ node_uuid: "n-2", followers: [{ node_uuid: "n-3" }] }]);
});

test("an automatic PD request carries placement_mode auto and the strategy; a manual one carries neither", () => {
  const auto = deployDraftPayload(pdDraft({ placement_strategy: "binpack", node_uuid: "stale" }), []);
  assert.equal(auto?.placement_mode, "auto");
  assert.equal(auto?.placement_strategy, "binpack");
  assert.equal("node_uuid" in (auto ?? {}), false);
  assert.equal("node_uuids" in (auto ?? {}), false);
  const manual = deployDraftPayload(pdDraft({ units_placement: "manual", placement_strategy: "binpack" }), []);
  assert.equal("placement_mode" in (manual ?? {}), false, "a PD request naming its units' nodes does not say auto");
  assert.equal("placement_strategy" in (manual ?? {}), false);
});

test("an automatic multi-node request names no node and keeps placement.tp", () => {
  const body = deployDraftPayload(multiDraft({ placement_strategy: "binpack", gpu_indexes: [0, 1] }), []) as Record<string, unknown>;
  assert.equal(body.placement_mode, "auto");
  assert.equal(body.placement_strategy, "binpack");
  assert.deepEqual(body.placement, { tp: 8 });
  assert.equal(body.nnodes, 2);
  assert.equal(body.dist_role, "leader");
  assert.equal(body.master_port, 29500);
  for (const key of ["node_uuids", "node_uuid", "master_addr"]) assert.equal(key in body, false, `${key} is the platform's to pick`);
});

test("a manual multi-node request names its nodes as before", () => {
  const body = deployDraftPayload(multiDraft({ units_placement: "manual", placement_strategy: "binpack", gpu_indexes: [0, 1] }), []) as Record<string, unknown>;
  assert.equal(body.placement_mode, "manual");
  assert.deepEqual(body.node_uuids, ["n-1", "n-2"]);
  assert.equal(body.node_uuid, "n-1");
  assert.equal(body.master_addr, "10.0.0.1");
  assert.deepEqual(body.placement, { tp: 8, gpus: [0, 1] });
  assert.equal("placement_strategy" in body, false);
});

test("a single-node draft is unchanged: strategy only for the pool, mode by its own position", () => {
  const auto = deployDraftPayload({ model_id: 3, node_selection_mode: "auto", nnodes: 1, placement_strategy: "spread", units_placement: "auto" }, []) as Record<string, unknown>;
  assert.equal(auto.placement_mode, "auto");
  assert.equal(auto.placement_strategy, "spread");
  assert.equal(buildPayload({ node_selection_mode: "manual", nnodes: 1, node_uuid: "n-1" }).payload.placement_mode, undefined, "buildPayload states a mode only for a platform-placed group");
});

test("a stored multi-node or PD draft reads as manual unless it says auto", () => {
  const multi = { nnodes: 2, node_uuids: ["n-1", "n-2"], placement: { tp: 4 } };
  assert.equal(draftToFormValues(multi).units_placement, "manual");
  assert.equal(draftToFormValues({ ...multi, placement_mode: "auto" }).units_placement, "auto");
  const pd = { placement: { pd: { mode: "vllm-nixl", roles: { decode: { tp: 1, units: [{ node_uuid: "n-1" }] } } } } };
  assert.equal(draftToFormValues(pd).units_placement, "manual");
  assert.equal(draftToFormValues({ ...pd, placement_mode: "auto" }).units_placement, "auto");
  assert.equal(draftToFormValues({ nnodes: 1, placement_mode: "auto" }).units_placement, undefined, "a single node has no units placement");
});

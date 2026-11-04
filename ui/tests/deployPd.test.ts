import assert from "node:assert/strict";
import test from "node:test";

import {
  buildPdPlacement,
  defaultPdMode,
  defaultPdRoles,
  pdActive,
  pdModeOf,
  pdModesForEngine,
  pdPolicyValue,
  pdRoleRemovable,
} from "../src/pages/deploy/deployPd.ts";
import { fixturePdModes } from "./helpers/engineFixture.ts";

const vllmModes = fixturePdModes("vllm");
const sglangModes = fixturePdModes("sglang");

test("pdActive reads the topology field, and only that field", () => {
  assert.equal(pdActive({ node_selection_mode: "pd" }), true);
  assert.equal(pdActive({ node_selection_mode: "auto" }), false);
  assert.equal(pdActive({ node_selection_mode: "multi" }), false);
  assert.equal(pdActive({}), false);
});

test("pdModesForEngine reads pd_modes straight off the engine profile", () => {
  assert.deepEqual(pdModesForEngine({ pd_modes: vllmModes }).map((mode) => mode.name), ["vllm-ascend-mooncake", "vllm-nixl"]);
  assert.deepEqual(pdModesForEngine({ pd_modes: sglangModes }).map((mode) => mode.name), ["sglang-nixl", "sglang-ppu-ipc"]);
  assert.deepEqual(pdModesForEngine({ pd_modes: [] }), []);
  assert.deepEqual(pdModesForEngine({}), []);
  assert.deepEqual(pdModesForEngine(undefined), []);
  assert.equal(pdModeOf({ pd_modes: vllmModes }, "vllm-nixl")?.tp_rule, "decode_at_least_prefill");
  assert.equal(pdModeOf({ pd_modes: vllmModes }, "sglang-nixl"), undefined);
});

test("defaultPdMode picks the mode the server declares for the chosen vendor", () => {
  const profile = { pd_modes: vllmModes };
  assert.equal(defaultPdMode(profile, "ascend"), "vllm-ascend-mooncake");
  assert.equal(defaultPdMode(profile, "nvidia"), "vllm-nixl");
  // SGLang: PPU gets its same-host mode, NVIDIA keeps NIXL.
  assert.equal(defaultPdMode({ pd_modes: sglangModes }, "ppu"), "sglang-ppu-ipc");
  assert.equal(defaultPdMode({ pd_modes: sglangModes }, "nvidia"), "sglang-nixl");
});

test("defaultPdMode without a matching vendor prefers a verified mode, then the first", () => {
  const unverifiedFirst = { pd_modes: [{ ...vllmModes[1], name: "x-unverified", unverified: true }, vllmModes[1]] };
  assert.equal(defaultPdMode(unverifiedFirst, undefined), "vllm-nixl");
  // No SGLang mode serves Ascend: the verified PPU mode beats the unverified NIXL one.
  assert.equal(defaultPdMode({ pd_modes: sglangModes }, "ascend"), "sglang-ppu-ipc");
});

test("defaultPdMode returns undefined for an engine with no PD modes", () => {
  assert.equal(defaultPdMode({ pd_modes: [] }, "nvidia"), undefined);
  assert.equal(defaultPdMode(undefined, "nvidia"), undefined);
});

test("pdPolicyValue keeps an allowed pick and otherwise takes the mode's default", () => {
  const nixl = pdModeOf({ pd_modes: vllmModes }, "vllm-nixl")!;
  const sglang = sglangModes[0];
  assert.equal(pdPolicyValue(nixl.fallbacks, undefined), "decode");
  assert.equal(pdPolicyValue(nixl.fallbacks, "fail"), "fail");
  // SGLang's decoder cannot serve alone: "decode" is not an option there.
  assert.equal(pdPolicyValue(sglang.fallbacks, "decode"), "fail");
  assert.equal(pdPolicyValue(nixl.kv_load_failures, undefined), "fail");
  // SGLang has no KV load failure setting, so nothing is sent.
  assert.equal(pdPolicyValue(sglang.kv_load_failures, "fail"), undefined);
});

test("pdRoleRemovable lets only a role the mode scales to zero lose its last unit", () => {
  const nixl = pdModeOf({ pd_modes: vllmModes }, "vllm-nixl");
  assert.equal(pdRoleRemovable(nixl, "prefill", 1), true);
  assert.equal(pdRoleRemovable(nixl, "decode", 1), false);
  assert.equal(pdRoleRemovable(nixl, "decode", 2), true);
  assert.equal(pdRoleRemovable(sglangModes[0], "prefill", 1), false);
  // Before the catalog has loaded every role keeps a unit.
  assert.equal(pdRoleRemovable(undefined, "prefill", 1), false);
});

test("defaultPdRoles seeds one unit per role at TP 1", () => {
  const roles = defaultPdRoles();
  assert.deepEqual(roles, {
    decode: { tp: 1, units: [{}] },
    prefill: { tp: 1, units: [{}] },
  });
});

test("buildPdPlacement returns undefined outside the pd topology", () => {
  assert.equal(
    buildPdPlacement({
      node_selection_mode: "manual",
      pd_mode: "vllm-nixl",
      pd_roles: defaultPdRoles(),
    }),
    undefined,
  );
});

test("buildPdPlacement returns undefined without a mode", () => {
  assert.equal(
    buildPdPlacement({
      node_selection_mode: "pd",
      pd_roles: defaultPdRoles(),
    }),
    undefined,
  );
});

test("buildPdPlacement projects mode, roles, fallback and kv_load_failure onto the wire shape", () => {
  const placement = buildPdPlacement({
    node_selection_mode: "pd",
    units_placement: "manual",
    pd_mode: "vllm-nixl",
    pd_fallback: "fail",
    pd_kv_load_failure: "recompute",
    pd_roles: {
      decode: { tp: 2, units: [{ node_uuid: "n1", gpus: [0, 1] }] },
      prefill: {
        tp: 1,
        units: [{ node_uuid: "n1", gpus: [2] }, { node_uuid: "n2" }],
      },
    },
  });
  assert.deepEqual(placement, {
    mode: "vllm-nixl",
    fallback: "fail",
    kv_load_failure: "recompute",
    roles: {
      decode: { units: [{ node_uuid: "n1", gpus: [0, 1] }], tp: 2 },
      prefill: {
        units: [{ node_uuid: "n1", gpus: [2] }, { node_uuid: "n2" }],
        tp: 1,
      },
    },
  });
});

test("buildPdPlacement keeps a unit row with no node yet - the count on the wire matches what the screen shows", () => {
  const placement = buildPdPlacement({
    node_selection_mode: "pd",
    units_placement: "manual",
    pd_mode: "vllm-nixl",
    pd_roles: {
      decode: { tp: 1, units: [{}] },
      prefill: { tp: 1, units: [{ node_uuid: "n1" }, {}] },
    },
  }) as { roles: { prefill: { units: unknown[] } } };
  assert.equal(placement.roles.prefill.units.length, 2);
  assert.deepEqual(placement.roles.prefill.units[1], {});
});

test("buildPdPlacement omits an empty gpus array rather than sending one", () => {
  const placement = buildPdPlacement({
    node_selection_mode: "pd",
    units_placement: "manual",
    pd_mode: "vllm-nixl",
    pd_roles: {
      decode: { tp: 1, units: [{ node_uuid: "n1", gpus: [] }] },
      prefill: { tp: 1, units: [{ node_uuid: "n2" }] },
    },
  }) as { roles: { decode: { units: Array<Record<string, unknown>> } } };
  assert.deepEqual(placement.roles.decode.units[0], { node_uuid: "n1" });
});

test("buildPdPlacement projects a role's pp and a unit's followers onto the wire shape", () => {
  const placement = buildPdPlacement({
    node_selection_mode: "pd",
    units_placement: "manual",
    pd_mode: "vllm-nixl",
    pd_roles: {
      decode: {
        tp: 4,
        pp: 2,
        units: [
          {
            node_uuid: "n1",
            gpus: [0, 1],
            followers: [{ node_uuid: "n2", gpus: [0, 1] }],
          },
        ],
      },
      prefill: { tp: 1, units: [{ node_uuid: "n3" }] },
    },
  });
  assert.deepEqual(placement, {
    mode: "vllm-nixl",
    roles: {
      decode: {
        units: [{ node_uuid: "n1", gpus: [0, 1], followers: [{ node_uuid: "n2", gpus: [0, 1] }] }],
        tp: 4,
        pp: 2,
      },
      prefill: { units: [{ node_uuid: "n3" }], tp: 1 },
    },
  });
});

test("buildPdPlacement keeps a follower row with no node yet, and omits an empty followers array", () => {
  const placement = buildPdPlacement({
    node_selection_mode: "pd",
    units_placement: "manual",
    pd_mode: "vllm-nixl",
    pd_roles: {
      decode: {
        tp: 2,
        units: [
          { node_uuid: "n1", followers: [{}] },
          { node_uuid: "n2", followers: [] },
        ],
      },
      prefill: { tp: 1, units: [{ node_uuid: "n3" }] },
    },
  }) as { roles: { decode: { units: Array<Record<string, unknown>> } } };
  assert.deepEqual(placement.roles.decode.units[0], { node_uuid: "n1", followers: [{}] });
  assert.deepEqual(placement.roles.decode.units[1], { node_uuid: "n2" });
});

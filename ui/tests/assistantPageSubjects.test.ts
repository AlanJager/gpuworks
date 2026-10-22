import assert from "node:assert/strict";
import fs from "node:fs";
import { test } from "node:test";
import { createInstance } from "i18next";
import { deploymentSubjects, nodeSubjects } from "../src/assistant/pageSubjects.ts";

const i18n = createInstance();
await i18n.init({ lng: "en", resources: { en: { ai: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/ai.json", import.meta.url), "utf8")) } }, defaultNS: "ai", interpolation: { escapeValue: false } });
const t = i18n.getFixedT("en", "ai");

test("a deployment offers the scheduling task only while replicas wait, and its failed instances", () => {
  const healthy = deploymentSubjects({ id: 3, name: "qwen", instances: [{ uuid: "aaaaaaaa-1", state: "running" }] } as never, t);
  assert.deepEqual(healthy, [{ subject: { kind: "deployment", id: 3, label: "qwen" }, tasks: [], attention: undefined }]);

  const instances = Array.from({ length: 8 }, (_, i) => ({ uuid: `bad0000${i}-x`, state: "failed" }));
  const stuck = deploymentSubjects({ id: 3, name: "qwen", placement_status: { unscheduled: 2 }, instances } as never, t);
  assert.deepEqual(stuck[0].tasks, ["deployment.unscheduled"]);
  assert.equal(stuck[0].attention, "2 replicas are waiting for a node");
  assert.equal(stuck.length, 1 + 5, "at most five failed instances");
  assert.deepEqual(stuck[1], { subject: { kind: "instance", uuid: "bad00000-x", label: "bad00000" }, tasks: ["instance.diagnose"], attention: "Failed" });

  // A PD deployment's placement is its roles', not the pool's.
  const pd = deploymentSubjects({ id: 3, name: "pd", pd: {}, placement_status: { unscheduled: 1 } } as never, t);
  assert.deepEqual(pd[0].tasks, []);
});

test("a node points at being offline before broken cards", () => {
  const gpu = (health_status: string) => ({ health_status });
  const ok = nodeSubjects({ uuid: "n1", hostname: "gpu-a", state: "online", gpus: [gpu("OK")] } as never, t);
  assert.equal(ok[0].attention, undefined);
  assert.deepEqual(ok[0].tasks, ["node.health"]);
  const broken = nodeSubjects({ uuid: "n1", hostname: "gpu-a", state: "online", gpus: [gpu("OK"), gpu("Error"), gpu("Error"), gpu("Warning")] } as never, t);
  assert.equal(broken[0].attention, "2 accelerators report errors");
  const offline = nodeSubjects({ uuid: "n1", hostname: "gpu-a", state: "offline", gpus: [gpu("Error")] } as never, t);
  assert.equal(offline[0].attention, "The node is offline");
  const connecting = nodeSubjects({ uuid: "n1", hostname: "gpu-a", state: "connecting", gpus: [gpu("Error")] } as never, t);
  assert.equal(connecting[0].attention, "The node is connecting");
});

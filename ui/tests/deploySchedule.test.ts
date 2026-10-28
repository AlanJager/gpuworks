import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import { createInstance } from "i18next";

import { scheduleText } from "../src/pages/deploy/deploySchedule.ts";

const locale = (lng: string) =>
  JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lng}/deploy.json`, import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deploy: locale("en") }, zh: { deploy: locale("zh") } },
  defaultNS: "deploy",
  interpolation: { escapeValue: false },
});

test("structured scheduler messages translate with interpolation params", () => {
  const calls: Array<{ key: string; options: Record<string, unknown> }> = [];
  const t = ((key: string, options: Record<string, unknown>) => {
    calls.push({ key, options });
    return `tp=${options.tp}, heads=${options.head_count}`;
  }) as never;

  assert.equal(
    scheduleText(t, { code: "tp_head_count_not_divisible", params: { tp: 4, head_count: 6 } }),
    "tp=4, heads=6",
  );
  assert.deepEqual(calls, [{
    key: "schedule.tp_head_count_not_divisible",
    options: { tp: 4, head_count: 6, defaultValue: "" },
  }]);
});

test("historical scheduler strings remain unchanged", () => {
  const t = (() => "unexpected") as never;
  assert.equal(scheduleText(t, "历史调度说明"), "历史调度说明");
});

test("missing scheduler translations fall back to legacy message and then code", () => {
  const t = ((key: string) => key) as never;
  assert.equal(scheduleText(t, { code: "future_code", message: "Legacy fallback" }), "Legacy fallback");
  assert.equal(scheduleText(t, { code: "future_code" }), "future_code");
  assert.equal(scheduleText(t, {}), "");
});

// A delivery the deployment waits on - a peer transfer or an archive URL - used
// to fail with no trace the user could see. The rendered refusal has to name
// what failed and carry the ledger's own text, which is the part that says
// whether to retry or pick another source.
test("a failed image delivery names the node, the image and the reported failure", () => {
  for (const lng of ["en", "zh"]) {
    const t = i18n.getFixedT(lng, "deploy");
    const detail = "dial model sync target: 10.42.14.163:34787: connect: no route to host";
    const text = scheduleText(t, {
      code: "image_delivery_failed",
      params: { node: "gpu3", image: "gpustack/runner:v1", source: "peer", detail },
    });
    assert.match(text, /gpu3/, `${lng} refusal does not name the node`);
    assert.match(text, /gpustack\/runner:v1/, `${lng} refusal does not name the image`);
    assert.ok(text.includes(detail), `${lng} refusal drops the reported failure: ${text}`);
  }
});

// The ledger does not always have a message, and a reason omits params it has
// no value for; the message must not render a placeholder in their place.
test("a failed image delivery without a reported message renders no placeholder", () => {
  for (const lng of ["en", "zh"]) {
    const t = i18n.getFixedT(lng, "deploy");
    const text = scheduleText(t, {
      code: "image_delivery_failed",
      params: { node: "gpu3", image: "gpustack/runner:v1" },
    });
    assert.match(text, /gpu3/, `${lng} refusal does not name the node`);
    assert.doesNotMatch(text, /\{\{/, `${lng} refusal rendered an unfilled placeholder: ${text}`);
  }
});

// A model stuck on a node holds every replica the pool would put there. The
// refusal names the model and the node, carries the failure when the
// acquisition reported one, and renders no placeholder when it did not.
test("a model stuck on a node names it and carries the reported failure", () => {
  for (const lng of ["en", "zh"]) {
    const t = i18n.getFixedT(lng, "deploy");
    const detail = "send peer model archive: dial model sync target: ";
    const text = scheduleText(t, {
      code: "model_unavailable",
      params: { node: "gw-acc-c", model: "Qwen/Qwen3-0.6B", state: "failed", detail },
    });
    assert.match(text, /gw-acc-c/, `${lng} refusal does not name the node`);
    assert.match(text, /Qwen\/Qwen3-0\.6B/, `${lng} refusal does not name the model`);
    assert.ok(text.includes(detail.trim()), `${lng} refusal drops the reported failure: ${text}`);

    const bare = scheduleText(t, {
      code: "model_unavailable",
      params: { node: "gw-acc-c", model: "Qwen/Qwen3-0.6B", state: "download_required" },
    });
    assert.doesNotMatch(bare, /\{\{/, `${lng} refusal rendered an unfilled placeholder: ${bare}`);
  }
});

test("a storage refusal says which storage and why, in both languages", () => {
  const notOnNode = { code: "storage_unavailable", params: { storage: "lab-nfs", storage_id: 2, reason: "storage_not_on_node" } };
  const en = scheduleText(i18n.getFixedT("en", "deploy"), notOnNode);
  assert.match(en, /Storage lab-nfs does not apply to this node/);
  assert.match(en, /storage node condition/);
  const zh = scheduleText(i18n.getFixedT("zh", "deploy"), notOnNode);
  assert.match(zh, /存储 lab-nfs 不用在这台节点上/);
  assert.match(scheduleText(i18n.getFixedT("zh", "deploy"), { ...notOnNode, params: { ...notOnNode.params, reason: "storage_unavailable" } }), /lab-nfs 在这台节点上挂载不可用/);
  // A reason without a specific text, or without a storage, keeps the general one.
  assert.match(scheduleText(i18n.getFixedT("en", "deploy"), { code: "storage_unavailable", params: { storage: "lab-nfs", reason: "shared_path_invalid" } }), /not ready on this node/);
  assert.match(scheduleText(i18n.getFixedT("en", "deploy"), { code: "storage_unavailable", params: { reason: "storage_not_on_node" } }), /not ready on this node/);
});

test("a source rule that forbids every image source names the node and the image and points to the rules", () => {
  const reason = { code: "image_source_forbidden", params: { node: "gpu-b-02", image: "docker.io/lmsysorg/sglang:v0.5.21" } };
  const en = scheduleText(i18n.getFixedT("en", "deploy"), reason);
  assert.match(en, /node gpu-b-02 forbid every source of image docker\.io\/lmsysorg\/sglang:v0\.5\.21/);
  assert.match(en, /Settings → Pull settings → Source rules/);
  const zh = scheduleText(i18n.getFixedT("zh", "deploy"), reason);
  assert.match(zh, /节点 gpu-b-02 适用的来源规则禁止了镜像 docker\.io\/lmsysorg\/sglang:v0\.5\.21/);
  assert.doesNotMatch(en + zh, /\{\{/);
});

test("a pinned PD unit waiting on cards other deployments hold names the node, the cards and the holders in both languages", () => {
  const reason = { code: "pinned_gpus_unavailable", params: { node: "gpu-a", gpus: "0, 1", holders: "chat, embed" } };
  const en = i18n.getFixedT("en", "deploy") as never;
  const zh = i18n.getFixedT("zh", "deploy") as never;
  assert.equal(
    scheduleText(en, reason),
    "GPU 0, 1 on gpu-a, which this unit is pinned to, is held by chat, embed. The unit waits for it; switch the deployment to automatic placement to let the platform use free GPUs instead.",
  );
  assert.equal(
    scheduleText(zh, reason),
    "该单元钉住的 gpu-a 上的 GPU 0, 1 正被 chat, embed 占用，单元会一直等待；把部署的放置方式改为自动，平台就会改用空闲的卡。",
  );
});

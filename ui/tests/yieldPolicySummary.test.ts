import assert from "node:assert/strict";
import { test } from "node:test";
import { createI18n } from "./helpers/dom.ts";
import { presetParams } from "../src/pages/yieldPolicy/model.ts";
import { paramsSummary, spanText, yieldSummary } from "../src/pages/yieldPolicy/summary.ts";

// The one phrase a policy is summed up in on the scheduling policy card, in
// both languages.

for (const lng of ["en", "zh"] as const) {
  const { i18n, locale } = await createI18n(["deployments"], lng);
  const t = i18n.getFixedT(lng, "deployments");
  const y = locale.deployments.yieldPolicy;

  test(`${lng}: a preset is summed up by its name`, () => {
    for (const name of ["resident", "online", "shared", "offline"] as const) {
      assert.equal(paramsSummary(t, presetParams[name]), y.preset.name[name]);
    }
  });

  test(`${lng}: a custom policy names its policy and the parameters that matter`, () => {
    const idle = paramsSummary(t, { policy: "idle", auto_sleep: false, idle_sec: 30, sleep_level: "l1", hold_max_sec: 120 });
    assert.equal(idle, i18n.t("deployments:yieldPolicy.summary.customDigest", {
      policy: y.policy.name.idle,
      digest: [i18n.t("deployments:yieldPolicy.summary.idle", { value: spanText(t, 30) }), y.summary.level.l1].join(y.summary.separator),
    }));
    const never = paramsSummary(t, { policy: "never", auto_sleep: true, idle_sec: 600, sleep_level: "l2", hold_max_sec: 120 });
    assert.ok(never.includes(y.policy.name.never) && never.includes(y.summary.autoSleep) && never.includes(y.summary.level.l2));
    // Anytime has no idle time of its own: the digest has no idle part.
    const anytime = paramsSummary(t, { policy: "anytime", auto_sleep: false, sleep_level: "l1", unfinished: "abort", hold_max_sec: 1800 });
    assert.ok(!anytime.includes(i18n.t("deployments:yieldPolicy.summary.idle", { value: "" }).trim()));
    assert.ok(anytime.includes(y.summary.level.l1));
  });

  test(`${lng}: a deployment with no policy is resident`, () => {
    assert.equal(yieldSummary(t, { id: 1 } as never), y.preset.name.resident);
  });

  test(`${lng}: spans read in days, hours or minutes when they divide evenly, else in seconds`, () => {
    assert.equal(spanText(t, 30), i18n.t("deployments:automation.duration.seconds", { value: 30 }));
    assert.equal(spanText(t, 90), i18n.t("deployments:automation.duration.seconds", { value: 90 }));
    assert.equal(spanText(t, 300), i18n.t("deployments:automation.duration.minutes", { value: 5 }));
    assert.equal(spanText(t, 7200), i18n.t("deployments:automation.duration.hours", { value: 2 }));
    assert.equal(spanText(t, 172800), i18n.t("deployments:automation.duration.days", { value: 2 }));
  });
}

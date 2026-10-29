import assert from "node:assert/strict";
import { test } from "node:test";
import { createI18n } from "./helpers/dom.ts";
import { paramsSummary, spanText, yieldSummary } from "../src/pages/yieldPolicy/summary.ts";

// The one line a policy is summed up in on the scheduling policy card, in both
// languages. It reads the saved parameters alone: no dictionary is involved.

const wording = {
  en: {
    never: "Never yield automatically",
    idle: "Yield when idle · yields after 1 min idle",
    timeslice: "Yield on a turn · idle 30 s / up to 2 min per turn",
    anytime: "Yield anytime · idle 30 s",
  },
  zh: {
    never: "不自动出让",
    idle: "闲置自动出让 · 闲置 1 分钟后让出",
    timeslice: "按时自动出让 · 闲置 30 秒 / 每轮最长 2 分钟",
    anytime: "随时出让 · 闲置 30 秒",
  },
} as const;

for (const lng of ["en", "zh"] as const) {
  const { i18n } = await createI18n(["deployments"], lng);
  const t = i18n.getFixedT(lng, "deployments");

  test(`${lng}: each policy is summed up by its name and the parameters that matter`, () => {
    assert.equal(paramsSummary(t, { policy: "never" }), wording[lng].never);
    assert.equal(paramsSummary(t, { policy: "idle", idle_sec: 60, sleep_level: "auto", hold_max_sec: 300 }), wording[lng].idle);
    assert.equal(
      paramsSummary(t, { policy: "timeslice", idle_sec: 30, sleep_level: "auto", max_serve_sec: 120, unfinished: "wait", unfinished_wait_sec: 30, hold_max_sec: 600 }),
      wording[lng].timeslice,
    );
    assert.equal(paramsSummary(t, { policy: "anytime", idle_sec: 30, sleep_level: "auto", unfinished: "abort", hold_max_sec: 3600 }), wording[lng].anytime);
  });

  test(`${lng}: the summary follows the saved values, not any default`, () => {
    assert.ok(paramsSummary(t, { policy: "idle", idle_sec: 90, sleep_level: "l2", hold_max_sec: 10 }).includes(spanText(t, 90)));
    assert.ok(paramsSummary(t, { policy: "timeslice", idle_sec: 7200, max_serve_sec: 86400, unfinished: "abort" }).includes(spanText(t, 86400)));
    // A policy saved without its parameters is named alone; never ignores any.
    assert.equal(paramsSummary(t, { policy: "timeslice" }), t("yieldPolicy.policy.name.timeslice"));
    assert.equal(paramsSummary(t, { policy: "never", idle_sec: 60 }), wording[lng].never);
  });

  test(`${lng}: a deployment with no policy never yields automatically`, () => {
    assert.equal(yieldSummary(t, { id: 1 } as never), wording[lng].never);
    assert.equal(yieldSummary(t, { id: 1, yield_policy: { policy: "idle", idle_sec: 60 } } as never), wording[lng].idle);
  });

  test(`${lng}: spans read in days, hours or minutes when they divide evenly, else in seconds`, () => {
    assert.equal(spanText(t, 30), i18n.t("deployments:automation.duration.seconds", { value: 30 }));
    assert.equal(spanText(t, 90), i18n.t("deployments:automation.duration.seconds", { value: 90 }));
    assert.equal(spanText(t, 300), i18n.t("deployments:automation.duration.minutes", { value: 5 }));
    assert.equal(spanText(t, 7200), i18n.t("deployments:automation.duration.hours", { value: 2 }));
    assert.equal(spanText(t, 172800), i18n.t("deployments:automation.duration.days", { value: 2 }));
  });
}

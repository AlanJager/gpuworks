import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { createInstance } from "i18next";
import { apiErrorText, apiResponseError } from "../src/apiError.ts";

for (const language of ["zh", "en"]) {
  test(`removed settings direct callers to the matching current workflow (${language})`, async () => {
    const i18n = createInstance();
    await i18n.init({ lng: language, resources: { [language]: { common: JSON.parse(readFileSync(new URL(`../src/i18n/locales/${language}/common.json`, import.meta.url), "utf8")) } }, defaultNS: "common" });
    const cases = [
      [{ setting: "default_model_source" }, language === "zh" ? /部署.*选择.*模型来源/ : /choose.*model source.*deploy/i],
      [{ setting: "image_registry_mirrors", route: "/api/v1/pull/source-rules" }, language === "zh" ? /设置 → 镜像与模型拉取.*来源规则/ : /Settings → Image & model pulls.*source rules/i],
      [{ setting: "network", route: "/api/v1/pull/proxies" }, language === "zh" ? /设置 → 镜像与模型拉取.*代理.*来源规则/ : /Settings → Image & model pulls.*prox.*source rules/i],
      [{ setting: "registry-credentials", route: "/api/v1/pull/credentials" }, language === "zh" ? /设置 → 镜像与模型拉取.*凭据/ : /Settings → Image & model pulls.*credential/i],
    ] as const;
    for (const [params, expected] of cases) {
      const error = await apiResponseError(new Response(JSON.stringify({ code: "SETTING_REMOVED", error: "Legacy English fallback", params }), { status: 410 }), "request failed");
      const message = apiErrorText(i18n.t.bind(i18n), error);
      assert.match(message, expected);
      if (params.setting === "default_model_source") assert.doesNotMatch(message, /拉取|pull/i, "model-source choice must not direct the user to image pulling");
    }
  });
}

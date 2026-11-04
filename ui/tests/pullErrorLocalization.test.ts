import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { createInstance } from "i18next";
import { APIError, apiErrorText, apiResponseError } from "../src/apiError.ts";
import { pullErrorText } from "../src/pages/settings/pull/pullFormat.ts";

const i18n = createInstance();
const resources = Object.fromEntries(["en", "zh"].map((language) => [language,
  Object.fromEntries(["settings", "common"].map((namespace) => [namespace,
    JSON.parse(readFileSync(new URL(`../src/i18n/locales/${language}/${namespace}.json`, import.meta.url), "utf8")),
  ])),
]));
await i18n.init({ resources, lng: "zh", fallbackLng: "en", interpolation: { escapeValue: false } });

for (const language of ["zh", "en"]) {
  test(`sync group validation wire has a fully localized reason in ${language}`, async () => {
    const wire = '{"code":"PULL_SETTING_INVALID","error":"name: a sync group needs a name","params":{"field":"name","detail":"a sync group needs a name","reason":"name_required"}}';
    const error = await apiResponseError(new Response(wire, { status: 400 }), "fallback");
    assert.ok(error instanceof APIError);
    const tPull = (key: string, options?: Record<string, unknown>) => String(i18n.t(key, { lng: language, ns: "settings", ...options }));
    const tCommon = (key: string, options: Record<string, unknown>) => String(i18n.t(key, { lng: language, ns: "common", ...options }));
    const text = pullErrorText(tPull, tCommon, error);
    assert.equal(text, language === "zh" ? "“组名”设置无效：请输入同步组名称" : "The “name” setting is invalid: Enter a sync group name");
    assert.ok(!apiErrorText(tCommon, error).includes("a sync group needs a name"));
  });

  test(`sync group constraint reasons preserve their parameters and translate in ${language}`, async () => {
    const tPull = (key: string, options?: Record<string, unknown>) => String(i18n.t(key, { lng: language, ns: "settings", ...options }));
    const tCommon = (key: string, options: Record<string, unknown>) => String(i18n.t(key, { lng: language, ns: "common", ...options }));
    for (const entry of [
      { field: "name", reason: "name_duplicate", zh: "该同步组名称已存在", en: "A sync group with this name already exists" },
      { field: "name", reason: "max_bytes", max: 64, zh: "最多允许 64 个 UTF-8 字节", en: "Use at most 64 UTF-8 bytes" },
      { field: "name", reason: "invalid_text", zh: "请输入有效文本，且不要包含控制字符", en: "Use valid text without control characters" },
      { field: "rate_limit", reason: "nonnegative", zh: "请输入零或正数", en: "Use zero or a positive value" },
      { field: "requirements", reason: "requirements_invalid", zh: "请检查适用节点条件", en: "Check the node selection conditions" },
    ]) {
      const params = { field: entry.field, reason: entry.reason, ...(entry.max ? { max: entry.max } : {}), detail: "original diagnostic" };
      const error = await apiResponseError(new Response(JSON.stringify({ code: "PULL_SETTING_INVALID", error: "English fallback", params }), { status: 400 }), "fallback");
      assert.ok(error instanceof APIError);
      Object.freeze(error.params);
      const expected = language === "zh" ? entry.zh : entry.en;
      assert.ok(pullErrorText(tPull, tCommon, error).endsWith(expected));
      assert.ok(apiErrorText(tCommon, error).endsWith(expected));
      assert.equal(error.params?.detail, "original diagnostic", "formatting preserves the wire diagnostic");
    }
  });

  test(`new and old unknown pull validation reasons have a localized fallback in ${language}`, () => {
    const tPull = (key: string, options?: Record<string, unknown>) => String(i18n.t(key, { lng: language, ns: "settings", ...options }));
    const tCommon = (key: string, options: Record<string, unknown>) => String(i18n.t(key, { lng: language, ns: "common", ...options }));
    for (const reason of [undefined, "future_reason"]) {
      const error = new APIError("PULL_SETTING_INVALID", { field: "name", reason, detail: "untranslated server reason" }, "English fallback", 400);
      const text = pullErrorText(tPull, tCommon, error);
      assert.ok(!text.includes("untranslated") && !text.includes("fallback"), text);
      assert.ok(!apiErrorText(tCommon, error).includes("untranslated"));
      assert.ok(text.includes(language === "zh" ? "请检查" : "Check"), text);
    }
  });
}

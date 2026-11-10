import assert from "node:assert/strict";
import test from "node:test";
import { localizedText } from "../src/i18n/localizedText.ts";

test("server-owned text resolves the current locale and its base language", () => {
  const text = { en: "English", zh: "中文" };
  assert.equal(localizedText(text, "zh-CN"), "中文");
  assert.equal(localizedText(text, "en_US"), "English");
});

test("server-owned text falls back to English and then the first non-empty translation", () => {
  assert.equal(localizedText({ en: "English", zh: "中文" }, "fr"), "English");
  assert.equal(localizedText({ en: "", th: "ไทย" }, "fr"), "ไทย");
  assert.equal(localizedText({}, "en"), "");
  assert.equal(localizedText(undefined, "en"), "");
});

test("legacy single-string server text remains wire-compatible", () => {
  assert.equal(localizedText("Legacy label", "zh"), "Legacy label");
});

test("regional catalog labels match a base UI language before English", () => {
  assert.equal(localizedText({ en: "Reference audio", "zh-CN": "参考音频" }, "zh"), "参考音频");
  assert.equal(localizedText({ en: "Reference audio", "zh-TW": "參考音訊" }, "zh"), "參考音訊");
});

test("exact locale and explicit base labels take precedence over regional alternatives", () => {
  const text = { "zh-TW": "繁體", zh: "通用中文", "zh-CN": "简体", en: "English" };
  assert.equal(localizedText(text, "zh-CN"), "简体");
  assert.equal(localizedText(text, "zh"), "通用中文");
  assert.equal(localizedText(text, "zh-HK"), "通用中文");
});

test("regional fallback normalizes tags and skips blank translations", () => {
  const text = { "zh-TW": "  ", " ZH_cn ": "参考音频", en: "English" };
  assert.equal(localizedText(text, " zh_HK "), "参考音频");
  assert.equal(localizedText({ zh: "", ...text }, "zh"), "参考音频");
});

test("regional alternatives keep declaration order without overriding unrelated language fallback", () => {
  const text = { "zh-TW": "繁體", "zh-CN": "简体", en: "English" };
  assert.equal(localizedText(text, "zh"), "繁體");
  assert.equal(localizedText(text, "en-US"), "English");
  assert.equal(localizedText(text, "fr"), "English");
});

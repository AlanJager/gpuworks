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

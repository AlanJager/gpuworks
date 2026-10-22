import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import i18next from "i18next";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

function locale(language: "en" | "zh") {
  return JSON.parse(fs.readFileSync(path.join(root, "src/i18n/locales", language, "deploy.json"), "utf8"));
}

test("quick deploy GPU counts use English plurals and Chinese classifiers", async () => {
  const i18n = i18next.createInstance();
  await i18n.init({
    lng: "en",
    fallbackLng: "en",
    resources: {
      en: { deploy: locale("en") },
      zh: { deploy: locale("zh") },
    },
    defaultNS: "deploy",
    interpolation: { escapeValue: false },
  });

  assert.equal(i18n.t("quick.autoNode", { node: "gpu3", count: 1 }), "Automatically use gpu3 · 1 GPU");
  assert.equal(i18n.t("quick.autoNode", { node: "gpu3", count: 8 }), "Automatically use gpu3 · 8 GPUs");
  assert.equal(i18n.t("quick.gpuCount", { count: 1 }), "1 GPU");
  assert.equal(i18n.t("quick.gpuCount", { count: 8 }), "8 GPUs");

  await i18n.changeLanguage("zh");
  assert.equal(i18n.t("quick.autoNode", { node: "gpu3", count: 8 }), "将自动使用 gpu3 · 8 张 GPU");
  assert.equal(i18n.t("quick.gpuCount", { count: 8 }), "8 张 GPU");
});

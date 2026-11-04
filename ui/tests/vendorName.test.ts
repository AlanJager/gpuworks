import assert from "node:assert/strict";
import { test } from "node:test";
import { createInstance } from "i18next";
import en from "../src/i18n/locales/en/common.json";
import zh from "../src/i18n/locales/zh/common.json";
import { vendorName } from "../src/vendorName.ts";
import { vendorLabel } from "../src/pages/gpus/gpuFormat.ts";

test("accelerator choice and GPU list use the same common locale dictionary", async () => {
  for (const [lng, common] of Object.entries({ en, zh })) {
    const i18n = createInstance();
    await i18n.init({ lng, resources: { [lng]: { common } }, defaultNS: "gpus", initImmediate: false });
    for (const name of ["amd", "nvidia", "ascend", "hygon", "ppu", "apple"]) {
      assert.equal(vendorName(name, i18n.t), common.vendor[name as keyof typeof common.vendor]);
      assert.equal(vendorLabel(name, i18n.t), vendorName(name, i18n.t));
    }
    assert.equal(vendorName("future-vendor", i18n.t), "future-vendor");
  }
});

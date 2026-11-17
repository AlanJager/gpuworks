import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import { createInstance } from "i18next";
import { APIError, apiErrorText } from "../src/apiError.ts";

for (const language of ["en", "zh"]) {
  test(`network refusals name both supported accelerator transports (${language})`, async () => {
    const load = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${language}/${ns}.json`, import.meta.url), "utf8"));
    const i18n = createInstance();
    await i18n.init({ lng: language, resources: { [language]: { common: load("common"), networks: load("networks") } }, defaultNS: "common", interpolation: { escapeValue: false } });
    const t = i18n.getFixedT(language, "common");
    for (const code of ["COMPUTE_NETWORK_ACCELERATOR_REQUIRED", "COMPUTE_NETWORK_NO_CARDS", "PD_NETWORK_ACCELERATOR_REQUIRED", "PD_NETWORK_HOST_REQUIRED", "NETWORK_TYPE_INVALID"]) {
      const shown = apiErrorText(t, new APIError(code, { node: "node-a", network: "fabric-a", mode: "vllm-ascend-mooncake" }, "fallback", 400));
      assert.match(shown, /RDMA/, code);
      assert.match(shown, /HCCS/, `${code} must not direct an HCCS operator to the wrong network type`);
      assert.doesNotMatch(shown, /\$t\(|typeGroup\.|networks:/, "all names must resolve in the actual error translator");
    }
    const invalid = apiErrorText(t, new APIError("NETWORK_TYPE_INVALID", {}, "fallback", 400));
    assert.match(invalid, language === "en" ? /Host Ethernet/ : /主机以太网/);
    assert.match(invalid, language === "en" ? /Host RDMA/ : /主机 RDMA/);
    const form = i18n.getFixedT(language, "networks");
    assert.match(form("form.roleNotOnType"), /HCCS/);
    assert.match(form("form.typeHelp"), /HCCS/);
    i18n.addResource(language, "networks", "type.accelerator_hccs", "renamed-accelerator-fabric");
    const revised = apiErrorText(t, new APIError("COMPUTE_NETWORK_ACCELERATOR_REQUIRED", { node: "node-a", network: "fabric-a" }, "fallback", 400));
    assert.match(revised, /renamed-accelerator-fabric/, "errors must use the same canonical type name as the network selector");
    assert.match(form("form.roleNotOnType"), /renamed-accelerator-fabric/);
    assert.match(form("form.typeHelp"), /renamed-accelerator-fabric/);
  });
}

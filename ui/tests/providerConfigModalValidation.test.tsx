import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import "./helpers/dom.ts";

const { act, cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { message } = await import("antd");
const { default: ProviderConfigModal } = await import("../src/components/ProviderConfigModal.tsx");
const { default: i18n } = await import("../src/i18n/index.ts");
type KindInfo = import("../src/api.ts").ProviderKindInfo;

for (const language of ["en", "zh"]) {
  for (const namespace of ["common", "providers"]) {
    i18n.addResourceBundle(language, namespace, JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${language}/${namespace}.json`, import.meta.url), "utf8")));
  }
}
const originalFetch = globalThis.fetch;
afterEach(async () => { await act(async () => { message.destroy(); cleanup(); }); globalThis.fetch = originalFetch; });

const kindInfos: KindInfo[] = [{
  kind: "zstack", label: { en: "ZStack" }, server_dial_supported: false,
  capabilities: { can_list_instances: true, can_power_control: false, can_install_agent: true, can_create: false, can_delete: false, can_inject_user_data: false, has_pricing: false },
  credential_fields: [
    { name: "endpoint", label: { en: "API Endpoint" }, secret: false, required: true },
    { name: "public_key", label: { en: "AccessKey ID" }, secret: false, required: true },
    { name: "private_key", label: { en: "AccessKey Secret" }, secret: true, required: true },
  ],
}];
const input = (id: string) => document.getElementById(id) as HTMLInputElement;

for (const language of ["en", "zh"] as const) {
  test(`ZStack add shows localized failures, keeps entered credentials and permits retry (${language})`, async () => {
    await i18n.changeLanguage(language);
    let saved = 0;
    let code = "ZSTACK_ACCESS_KEY_INVALID";
    globalThis.fetch = async (url, init) => {
      assert.equal(url, "/api/v1/providers");
      assert.equal(init?.method, "POST");
      const body = JSON.parse(String(init?.body));
      assert.equal(body.public_key, "entered-ak");
      assert.equal(body.private_key, "entered-sk");
      return code
        ? new Response(JSON.stringify({ code, error: "English server fallback" }), { status: 400 })
        : new Response('{"id":1,"kind":"zstack","name":"cloud"}', { status: 201 });
    };
    render(<I18nextProvider i18n={i18n}><ProviderConfigModal open defaultKind="zstack" kindInfos={kindInfos} providerLabel="ZStack" onClose={() => {}} onSaved={() => { saved++; }} /></I18nextProvider>);
    await waitFor(() => assert.ok(input("endpoint")));
    for (const [name, value] of Object.entries({ name: "cloud", endpoint: "http://cloud.example.test:8080/zstack", public_key: "entered-ak", private_key: "entered-sk" })) {
      fireEvent.change(input(name), { target: { value } });
    }
    const addButton = () => screen.getByRole("button", { name: (name) => name.replace(/\s/g, "") === i18n.t("config.add", { ns: "providers" }).replace(/\s/g, "") });
    for (const [nextCode, zh, en] of [
      ["ZSTACK_ACCESS_KEY_INVALID", "AK 不存在或已禁用", "AK does not exist or is disabled"],
      ["ZSTACK_PORT_REFUSED", "端口连接被拒绝", "port refused the connection"],
      ["ZSTACK_IP_UNREACHABLE", "无法到达 ZStack IP", "cannot reach the ZStack IP"],
      ["ZSTACK_NETWORK_UNCONFIRMED", "目前无法确定具体原因", "the cause is not yet determined"],
      ["ZSTACK_NETWORK_DIAGNOSTIC_UNAVAILABLE", "IP 探测无法执行", "IP probe could not run"],
      ["ZSTACK_LICENSE_REQUIRED", "有效的企业版许可证", "valid Enterprise license"],
      ["ZSTACK_RESPONSE_TIMEOUT", "API 响应超时", "API response timed out"],
      ["ZSTACK_REQUEST_TIMEOUT", "API 请求超时", "API request timed out"],
    ]) {
      code = nextCode;
      fireEvent.click(addButton());
      await waitFor(() => assert.ok(screen.getByText(new RegExp(language === "zh" ? zh : en))));
      assert.equal(saved, 0, "failed validation must leave the dialog open");
      assert.equal(input("private_key").value, "entered-sk");
      assert.equal(input("endpoint").value, "http://cloud.example.test:8080/zstack");
      assert.equal(screen.queryByText(/English server fallback/), null);
      await waitFor(() => assert.ok(!addButton().classList.contains("ant-btn-loading")));
    }
    code = "";
    fireEvent.click(addButton());
    await waitFor(() => assert.equal(saved, 1));
    assert.equal(input("private_key")?.value ?? "", "");
  });
}

import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
const { i18n } = await createI18n(["providers"], "en");
const { act, cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { default: ProviderConfigModal } = await import("../src/components/ProviderConfigModal.tsx");
const { api } = await import("../src/api.ts");
type Config = import("../src/api.ts").ProviderEditConfig;
type Provider = import("../src/api.ts").CloudProvider;
type KindInfo = import("../src/api.ts").ProviderKindInfo;
const original = { ...api };
afterEach(() => { cleanup(); Object.assign(api, original); });
const kindInfos: KindInfo[] = [{
  kind: "compshare", label: { en: "CompShare" }, server_dial_supported: true,
  capabilities: { can_list_instances: true, can_power_control: true, can_install_agent: true, can_create: true, can_delete: true, can_inject_user_data: false, has_pricing: true },
  credential_fields: [
    { name: "endpoint", label: { en: "API Endpoint" }, placeholder: { en: "https://api.compshare.cn" }, secret: false, required: false },
    { name: "public_key", label: { en: "Public Key" }, secret: false, required: true },
    { name: "private_key", label: { en: "Private Key" }, secret: true, required: true },
  ],
}];
const saved: Config = { id: 1, kind: "compshare", name: "account", endpoint: "https://proxy.example.test:8443/cloud", public_key: "public-id", extra: {} };
const provider = (id = 1): Provider => ({ id, name: "list-name", kind: "compshare", state: "valid", instance_count: 0, created_at: "", updated_at: "" });
const view = (open = true, id = 1, kinds = kindInfos) => <I18nextProvider i18n={i18n}><ProviderConfigModal open={open} provider={provider(id)} kindInfos={kinds} providerLabel="Public Cloud" onClose={() => {}} onSaved={() => {}} /></I18nextProvider>;
const input = (name: string) => screen.getByLabelText(name) as HTMLInputElement;
const save = () => screen.getByRole("button", { name: /Save/ }) as HTMLButtonElement;

test("edit waits for saved config, displays proxy and public key, and leaves secrets blank", async () => {
  let resolve!: (value: Config) => void;
  api.provider = () => new Promise((done) => { resolve = done; });
  render(view());
  assert.equal(save().disabled, true);
  assert.equal(input("API Endpoint").disabled, true);
  await act(async () => resolve(saved));
  await waitFor(() => assert.equal(save().disabled, false));
  assert.equal(input("API Endpoint").value, saved.endpoint);
  assert.equal(input("Public Key").value, "public-id");
  assert.equal(input("Private Key").value, "");
  assert.equal(input("Name").value, "account");
});

test("changing only name retains the override and never invents secret values", async () => {
  api.provider = async () => saved;
  const calls: unknown[] = [];
  api.updateProvider = async (id, body) => { calls.push({ id, body }); return { id, kind: saved.kind, name: body.name }; };
  render(view());
  await waitFor(() => assert.equal(save().disabled, false));
  fireEvent.change(input("Name"), { target: { value: "renamed" } });
  fireEvent.click(save());
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(JSON.parse(JSON.stringify(calls[0])), { id: 1, body: { name: "renamed", endpoint: saved.endpoint, public_key: "public-id" } });
});

test("reopening reads saved values again, preserving empty endpoint instead of the placeholder", async () => {
  let current = saved;
  api.provider = async () => current;
  const rendered = render(view());
  await waitFor(() => assert.equal(input("API Endpoint").value, saved.endpoint));
  rendered.rerender(view(false));
  current = { ...saved, endpoint: "" };
  rendered.rerender(view());
  await waitFor(() => assert.equal(save().disabled, false));
  assert.equal(input("API Endpoint").value, "");
  assert.equal(input("API Endpoint").placeholder, "https://api.compshare.cn");
});

test("failed read blocks saving, and closing and reopening recovers", async () => {
  api.provider = async () => { throw new Error("unavailable"); };
  const calls: unknown[] = [];
  api.updateProvider = async (id, body) => { calls.push(body); return { id, kind: saved.kind, name: body.name }; };
  const rendered = render(view());
  await waitFor(() => assert.ok(screen.getByRole("alert")));
  assert.equal(save().disabled, true);
  fireEvent.click(save());
  assert.equal(calls.length, 0);
  rendered.rerender(view(false));
  api.provider = async () => saved;
  rendered.rerender(view());
  await waitFor(() => assert.equal(save().disabled, false));
  assert.equal(screen.queryByRole("alert"), null);
  assert.equal(input("API Endpoint").value, saved.endpoint);
});

test("late response from another account cannot overwrite the active account", async () => {
  let first!: (value: Config) => void;
  api.provider = (id) => id === 1 ? new Promise((done) => { first = done; }) : Promise.resolve({ ...saved, id: 2, endpoint: "https://second.example.test" });
  const rendered = render(view());
  rendered.rerender(view(true, 2));
  await waitFor(() => assert.equal(input("API Endpoint").value, "https://second.example.test"));
  await act(async () => first(saved));
  assert.equal(input("API Endpoint").value, "https://second.example.test");
});

test("schema arriving after detail cannot clear the saved endpoint, public key or name", async () => {
  api.provider = async () => saved;
  const rendered = render(view(true, 1, []));
  await waitFor(() => assert.equal(input("Name").value, "account"));
  assert.equal(save().disabled, true);
  rendered.rerender(view());
  await waitFor(() => assert.equal(input("API Endpoint").value, saved.endpoint));
  assert.equal(input("Public Key").value, "public-id");
  assert.equal(input("Name").value, "account");
});


test("schema-declared nonsecret extras are filled without exposing secret extras", async () => {
  const aliyun: KindInfo[] = [{ ...kindInfos[0], kind: "aliyun", credential_fields: [
    { name: "role_arn", label: { en: "Role ARN" }, secret: false, required: false, extra: true },
    { name: "regions", label: { en: "Regions" }, secret: false, required: false, extra: true },
    { name: "private_key", label: { en: "Access Key Secret" }, secret: true, required: true },
  ] }];
  api.provider = async () => ({ ...saved, kind: "aliyun", endpoint: "", extra: { role_arn: "acs:role/readonly", regions: "cn-a,cn-b" } });
  render(<I18nextProvider i18n={i18n}><ProviderConfigModal open provider={{ ...provider(), kind: "aliyun" }} kindInfos={aliyun} providerLabel="Public Cloud" onClose={() => {}} onSaved={() => {}} /></I18nextProvider>);
  await waitFor(() => assert.equal(save().disabled, false));
  assert.equal(input("Role ARN").value, "acs:role/readonly");
  assert.equal(input("Regions").value, "cn-a,cn-b");
  assert.equal(input("Access Key Secret").value, "");
});

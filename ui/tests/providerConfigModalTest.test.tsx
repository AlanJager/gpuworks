// The "Test connection" button of the Add / Edit External Platform dialog
// (ui/src/components/ProviderConfigModal.tsx): the request a click sends and
// what the server makes of it. A new account posts its form to
// /providers/test. A saved one always posts to /providers/{id}/test with what
// the form holds as the override: the form is prefilled with what
// GET /providers/{id} reads back (endpoint, public key, extra, never a
// secret), and the server keeps the stored value of every field left out.
// The server here answers as the real one does: it checks the credentials it
// ends up with, so a request that drops the stored secret fails.
import assert from "node:assert/strict";
import { afterEach, mock, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { field, framesSettled, freshPopupFramesPerTest } from "./helpers/platformDialog.ts";

const { i18n, locale } = await createI18n(["providers"], "en");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { api } = await import("../src/api.ts");
const { default: ProviderConfigModal } = await import("../src/components/ProviderConfigModal.tsx");

type Config = import("../src/api.ts").ProviderEditConfig;
type Provider = import("../src/api.ts").CloudProvider;
type KindInfo = import("../src/api.ts").ProviderKindInfo;
type Credentials = { endpoint?: string; api_key?: string; public_key?: string; private_key?: string; extra?: Record<string, string> };

const strings = locale.providers.test;
const apiError = locale.common.apiError;

const capabilities = { can_list_instances: true, can_power_control: false, can_install_agent: true, can_create: true, can_delete: false, can_inject_user_data: false, has_pricing: false };
// GET /providers/kinds, with the credential fields each driver declares (internal/server/provider).
const kindInfos: KindInfo[] = [
  {
    kind: "runpod", label: { en: "RunPod" }, category: "public_cloud", resources: [{ type: "container" }], server_dial_supported: false, capabilities,
    credential_fields: [
      { name: "endpoint", label: { en: "API Endpoint" }, secret: false, required: false },
      { name: "api_key", label: { en: "API Key" }, secret: true, required: true },
    ],
  },
  {
    kind: "zstack", label: { en: "ZStack Cloud" }, category: "private_cloud", resources: [{ type: "host" }], server_dial_supported: true, capabilities,
    credential_fields: [
      { name: "endpoint", label: { en: "API Endpoint" }, secret: false, required: true, default_template: "http://MANAGEMENT_NODE_IP:8080/zstack" },
      { name: "public_key", label: { en: "AccessKey ID" }, secret: false, required: true },
      { name: "private_key", label: { en: "AccessKey Secret" }, secret: true, required: true },
    ],
  },
  {
    kind: "compshare", label: { en: "CompShare" }, category: "public_cloud", resources: [{ type: "vm" }], server_dial_supported: false, capabilities,
    credential_fields: [
      { name: "public_key", label: { en: "Public Key" }, secret: false, required: true },
      { name: "private_key", label: { en: "Private Key" }, secret: true, required: true },
    ],
  },
  {
    kind: "aliyun", label: { en: "Alibaba Cloud" }, category: "public_cloud", resources: [{ type: "vm" }], server_dial_supported: false, capabilities,
    credential_fields: [
      { name: "public_key", label: { en: "AccessKey ID" }, secret: false, required: true },
      { name: "private_key", label: { en: "AccessKey Secret" }, secret: true, required: true },
      { name: "role_arn", label: { en: "Role ARN" }, secret: false, required: false, extra: true },
      { name: "regions", label: { en: "Regions" }, secret: false, required: false, extra: true },
    ],
  },
];

// What the server stores of each saved account. GET /providers/{id} reads back everything but the secrets.
const stored: Record<number, { kind: string; name: string; credentials: Credentials }> = {
  7: { kind: "runpod", name: "pods", credentials: { api_key: "rk-stored" } },
  8: { kind: "zstack", name: "lab-cloud", credentials: { endpoint: "http://10.0.0.5:8080/zstack", public_key: "zs-ak", private_key: "zs-stored-secret" } },
  9: { kind: "compshare", name: "ucloud", credentials: { public_key: "cs-public", private_key: "cs-stored-secret" } },
  10: { kind: "aliyun", name: "ali-prod", credentials: { public_key: "LTAI-ak", private_key: "ali-stored-secret", extra: { regions: "cn-shanghai" } } },
};
const savedConfig = (id: number): Config => {
  const { kind, name, credentials } = stored[id];
  return { id, kind, name, endpoint: credentials.endpoint ?? "", public_key: credentials.public_key ?? "", extra: { ...credentials.extra } };
};
const providerOf = (id: number): Provider => ({ id, name: stored[id].name, kind: stored[id].kind, category: "public_cloud", state: "valid", instance_count: 0, resources: [{ type: "vm" }], node_count: 0, created_at: "", updated_at: "" });

type Sent = { route: string; method?: string; contentType: string | null; body: unknown; hasBody: boolean; signal?: AbortSignal | null; tested?: Credentials };
const sse = (event: string, data: unknown) => `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`;

/**
 * POST /providers/test and POST /providers/{id}/test as the server answers
 * them: a saved account's override is merged over what is stored (a blank or
 * absent field is the stored value; no body is the stored credentials as they
 * are), then the kind's required credentials are checked on the result.
 */
function serveTests() {
  const sent: Sent[] = [];
  mock.method(globalThis, "fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
    const route = String(input);
    const hasBody = init?.body !== undefined && init?.body !== null;
    const body = hasBody ? JSON.parse(String(init?.body)) as Credentials & { kind?: string } : undefined;
    const record: Sent = { route, method: init?.method, contentType: new Headers(init?.headers).get("Content-Type"), body, hasBody, signal: init?.signal };
    sent.push(record);
    const saved = /^\/api\/v1\/providers\/(\d+)\/test$/.exec(route);
    if (!saved && route !== "/api/v1/providers/test") return new Response("not found", { status: 404 });
    let kind = body?.kind ?? "";
    let tested: Credentials = body ?? {};
    if (saved) {
      const account = stored[Number(saved[1])];
      kind = account.kind;
      const pick = (name: "endpoint" | "api_key" | "public_key" | "private_key") => body?.[name]?.trim() || account.credentials[name];
      tested = { endpoint: pick("endpoint"), api_key: pick("api_key"), public_key: pick("public_key"), private_key: pick("private_key"), extra: { ...account.credentials.extra, ...body?.extra } };
    }
    record.tested = tested;
    const missing = kindInfos.find((info) => info.kind === kind)?.credential_fields
      .find((f) => f.required && !f.extra && !(tested as Record<string, unknown>)[f.name]);
    const frames = missing
      ? sse("check", { seq: 1, name: "credentials_shape", status: "failed", duration_ms: 0, code: "PROVIDER_CREDENTIAL_MISSING", detail: { field: missing.name }, hint: "credential_missing" })
        + sse("done", { ok: false, failed: ["credentials_shape"] })
      : sse("check", { seq: 1, name: "credentials_shape", status: "ok", duration_ms: 0 })
        + sse("check", { seq: 2, name: "auth_request", status: "ok", duration_ms: 31 })
        + sse("done", { ok: true });
    return new Response(frames, { status: 200, headers: { "Content-Type": "text/event-stream" } });
  });
  return sent;
}

afterEach(() => { mock.restoreAll(); cleanup(); });
await freshPopupFramesPerTest();

async function openEdit(id: number) {
  mock.method(api, "provider", async (asked: number) => savedConfig(asked));
  render(<I18nextProvider i18n={i18n}><ProviderConfigModal open provider={providerOf(id)} kindInfos={kindInfos} providerLabel="Cloud Provider" onClose={() => {}} onSaved={() => {}} /></I18nextProvider>);
  // The dialog is locked until the saved configuration has been read; an enabled save says it has.
  await waitFor(() => assert.equal((screen.getByRole("button", { name: /Save/ }) as HTMLButtonElement).disabled, false));
}
async function openAdd() {
  render(<I18nextProvider i18n={i18n}><ProviderConfigModal open kindInfos={kindInfos} providerLabel="Cloud Provider" onClose={() => {}} onSaved={() => {}} /></I18nextProvider>);
  await waitFor(() => assert.equal(testButton().disabled, false, "a chosen platform enables the test"));
}
const input = (name: string) => screen.getByLabelText(field(name)) as HTMLInputElement;
const testButton = () => screen.getByRole("button", { name: strings.run }) as HTMLButtonElement;
/** The connection-test dialog: the one on top once a test is open. */
const testDialog = () => screen.getByRole("dialog", { name: strings.title });
/** The test requests, as sent. */
const tests = (sent: Sent[]) => sent.filter((request) => request.route.endsWith("/test"));

test("a saved ZStack account with nothing typed is tested on the saved-account route: the prefilled endpoint and key go as the override, no secret is sent, and the stored secret makes it pass", async () => {
  const sent = serveTests();
  await openEdit(8);
  assert.equal(input("API Endpoint").value, "http://10.0.0.5:8080/zstack", "the form shows what the server reads back");
  assert.equal(input("AccessKey ID").value, "zs-ak");
  assert.equal(input("AccessKey Secret").value, "", "a secret is never read back");
  fireEvent.click(testButton());
  await within(testDialog()).findByText(strings.ok);
  assert.equal(tests(sent).length, 1);
  const [request] = tests(sent);
  assert.equal(request.route, "/api/v1/providers/8/test");
  assert.equal(request.method, "POST");
  assert.equal(request.contentType, "application/json");
  assert.deepEqual(request.body, { endpoint: "http://10.0.0.5:8080/zstack", public_key: "zs-ak" }, "a blank secret is left out, not sent blank, and no kind is sent for a saved account");
  assert.equal(request.tested?.private_key, "zs-stored-secret", "the server tested the stored secret");
  assert.equal(within(testDialog()).queryByText(apiError.PROVIDER_CREDENTIAL_MISSING), null);
});

test("a saved ZStack account with a new secret and endpoint typed sends both, trimmed, and the server tests them in place of the stored ones", async () => {
  const sent = serveTests();
  await openEdit(8);
  fireEvent.change(input("AccessKey Secret"), { target: { value: "  zs-new-secret  " } });
  fireEvent.change(input("API Endpoint"), { target: { value: "https://zstack.example:8443/zstack" } });
  fireEvent.click(testButton());
  await within(testDialog()).findByText(strings.ok);
  const [request] = tests(sent);
  assert.equal(request.route, "/api/v1/providers/8/test");
  assert.deepEqual(request.body, { endpoint: "https://zstack.example:8443/zstack", public_key: "zs-ak", private_key: "zs-new-secret" });
  assert.equal(request.tested?.private_key, "zs-new-secret");
  assert.equal(request.tested?.endpoint, "https://zstack.example:8443/zstack");
});

test("a saved CompShare account, which has no endpoint, sends its public key alone and passes on the stored secret; a typed secret is sent", async () => {
  const sent = serveTests();
  await openEdit(9);
  assert.equal(input("Public Key").value, "cs-public");
  fireEvent.click(testButton());
  await within(testDialog()).findByText(strings.ok);
  assert.equal(tests(sent)[0].route, "/api/v1/providers/9/test");
  assert.deepEqual(tests(sent)[0].body, { public_key: "cs-public" });
  assert.equal(tests(sent)[0].tested?.private_key, "cs-stored-secret");

  // Close the result, type a new secret and test again.
  const user = userEvent.setup({ document: dom.window.document });
  await framesSettled();
  await user.keyboard("{Escape}");
  await waitFor(() => assert.equal(screen.queryByRole("dialog", { name: strings.title }), null));
  await waitFor(() => assert.equal(testButton().disabled, false));
  fireEvent.change(input("Private Key"), { target: { value: "cs-new-secret" } });
  fireEvent.click(testButton());
  await within(testDialog()).findByText(strings.ok);
  assert.deepEqual(tests(sent)[1].body, { public_key: "cs-public", private_key: "cs-new-secret" });
});

test("a saved Alibaba Cloud account sends its key and the extra fields it reads back, never the secret", async () => {
  const sent = serveTests();
  await openEdit(10);
  fireEvent.click(testButton());
  await within(testDialog()).findByText(strings.ok);
  assert.equal(tests(sent)[0].route, "/api/v1/providers/10/test");
  assert.deepEqual(tests(sent)[0].body, { public_key: "LTAI-ak", extra: { regions: "cn-shanghai" } });
});

test("a saved account whose form holds nothing to send (RunPod: only a secret, left blank) is tested with no body at all", async () => {
  const sent = serveTests();
  await openEdit(7);
  fireEvent.click(testButton());
  await within(testDialog()).findByText(strings.ok);
  const [request] = tests(sent);
  assert.equal(request.route, "/api/v1/providers/7/test");
  assert.equal(request.hasBody, false, "no override, no body");
  assert.equal(request.contentType, null);
  assert.equal(request.tested?.api_key, "rk-stored");
});

test("adding a platform posts the form with its kind to /providers/test: there are no stored credentials", async () => {
  const sent = serveTests();
  await openAdd();
  fireEvent.change(input("API Key"), { target: { value: " rk-secret " } });
  fireEvent.click(testButton());
  await within(testDialog()).findByText(strings.ok);
  const [request] = tests(sent);
  assert.equal(request.route, "/api/v1/providers/test");
  assert.equal(request.method, "POST");
  assert.deepEqual(request.body, { kind: "runpod", api_key: "rk-secret" }, "the value is trimmed, blank fields dropped");
});

test("adding a platform with its secret left blank fails on the missing credential and says so", async () => {
  const sent = serveTests();
  await openAdd();
  fireEvent.click(testButton());
  await within(testDialog()).findByText(strings.failed);
  assert.deepEqual(tests(sent)[0].body, { kind: "runpod" });
  assert.ok(within(testDialog()).getByText(apiError.PROVIDER_CREDENTIAL_MISSING));
  assert.ok(within(testDialog()).getByText(strings.hint.credential_missing));
});

test("an open test cannot be started twice", async () => {
  const sent = serveTests();
  await openEdit(8);
  fireEvent.click(testButton());
  // The open test dialog sits on top, so the modal behind it is inert and its buttons are hidden from role queries.
  const hiddenButton = () => screen.getByRole("button", { name: strings.run, hidden: true }) as HTMLButtonElement;
  await waitFor(() => assert.equal(hiddenButton().disabled, true, "the button is disabled while its test is open"));
  fireEvent.click(hiddenButton());
  fireEvent.click(hiddenButton());
  await within(testDialog()).findByText(strings.ok);
  assert.equal(tests(sent).length, 1, "one open test, one request");
});

test("closing the test while it runs aborts its request, and the form can start another", async () => {
  const signals: AbortSignal[] = [];
  // A server that never answers: the request ends only when its signal aborts, as fetch does.
  mock.method(globalThis, "fetch", (_input: RequestInfo | URL, init?: RequestInit) => new Promise<Response>((_resolve, reject) => {
    const signal = init?.signal as AbortSignal;
    signals.push(signal);
    signal.addEventListener("abort", () => reject(signal.reason));
  }));
  await openEdit(8);
  fireEvent.click(testButton());
  await within(testDialog()).findByRole("status");
  await waitFor(() => assert.equal(signals.length, 1));
  assert.equal(signals[0].aborted, false);
  const user = userEvent.setup({ document: dom.window.document });
  await framesSettled();
  await user.keyboard("{Escape}");
  await waitFor(() => assert.equal(signals[0].aborted, true, "closing ends the request"));
  await waitFor(() => assert.equal(screen.queryByRole("dialog", { name: strings.title }), null));
  assert.equal(screen.queryByText(strings.failed), null, "an aborted test is not reported as a failure");
  await waitFor(() => assert.equal(testButton().disabled, false, "the form is back, and its test can run again"));
});

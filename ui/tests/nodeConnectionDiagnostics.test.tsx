import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const en = await createI18n(["nodes", "common"], "en");
const zh = await createI18n(["nodes", "common"], "zh");
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { nodeConnectionApi } = await import("../src/nodeConnectionApi.ts");
const View = (await import("../src/components/NodeConnectionDiagnostics.tsx")).default;
type Report = import("../src/nodeConnectionApi.ts").NodeConnectionDiagnostics;
const original = { me: api.me, diagnose: nodeConnectionApi.diagnose };
afterEach(() => { cleanup(); api.me = original.me; nodeConnectionApi.diagnose = original.diagnose; });

function report(uuid = "node-1"): Report {
  return {
    node_uuid: uuid, checked_at: "2026-10-23T10:19:00Z", connect_mode: "agent_dial",
    saved_server_url: "https://saved.example", last_seen_at: "2026-10-22T05:43:00Z",
    recovery_phase: "probing", recovery_attempts: 10, last_probe_at: "2026-10-23T10:17:24Z", last_recovery_error: "agent has no control session",
    exposure: { url: "https://current.example", state: "ready", observed_at: "2026-10-23T10:00:00Z" },
    checks: [
      { stage: "ssh", outcome: "connected", vantage: "control_plane", observed_at: "2026-10-23T10:19:00Z" },
      { stage: "configuration", outcome: "snapshot", vantage: "node", source: "state_file", target: "https://node-snapshot.example", observed_at: "2026-10-23T10:19:01Z" },
      { stage: "node_http", outcome: "dns", vantage: "node", target: "https://saved.example/healthz", observed_at: "2026-10-23T10:19:02Z" },
    ],
    guidance: ["addresses_differ", "configuration_differs", "snapshot_scope", "http_scope", "test_from_node", "manual_address_update"],
  };
}

const show = (bundle = en, nodeUUID = "node-1") => render(<I18nextProvider i18n={bundle.i18n}><View nodeUUID={nodeUUID} /></I18nextProvider>);
const admin = () => { api.me = async () => ({ role: "admin" }) as never; };

for (const bundle of [en, zh]) {
  test(`connection evidence shows location, addresses, historical time and bounded conclusions in ${bundle.i18n.language}`, async () => {
    admin(); nodeConnectionApi.diagnose = async () => report();
    const text = bundle.locale.nodes.connectionDiagnostics;
    show(bundle);
    const button = await screen.findByRole("button", { name: text.run });
    assert.ok(screen.getByText(text.empty));
    button.focus(); await userEvent.setup().keyboard("{Enter}");
    await screen.findByText("https://saved.example/healthz");
    assert.ok(screen.getByText("https://node-snapshot.example"));
    assert.ok(screen.getByText("https://current.example"));
    assert.ok(screen.getByText(text.outcome.dns));
    assert.ok(screen.getByText(text.outcome.snapshot));
    assert.ok(screen.getByText(text.location.control_plane));
    assert.ok(screen.getByText(text.guidance.addresses_differ));
    assert.ok(screen.getByText(text.guidance.manual_address_update));
    assert.ok(screen.getByText(text.guidance.http_scope));
    assert.equal(screen.queryByText(text.empty), null);
    assert.equal(screen.queryByText(/TOKEN=/), null);
  });
}

test("a loading check and a failed request have independent visible states without echoing unsafe transport text", async () => {
  admin(); let reject!: (error: Error) => void;
  nodeConnectionApi.diagnose = () => new Promise((_resolve, no) => { reject = no; });
  const text = en.locale.nodes.connectionDiagnostics;
  show(); await userEvent.setup().click(await screen.findByRole("button", { name: text.run }));
  assert.equal(screen.getByRole("status").textContent, text.loading);
  reject(new Error("https://user:password@host/?token=secret"));
  await screen.findByText(text.failed);
  assert.equal(screen.queryByRole("status"), null);
  assert.equal(document.body.textContent?.includes("token=secret"), false);
});

test("missing SSH and connector evidence remain unknown while health success has a restricted scope", async () => {
  admin(); const data = report();
  data.exposure = null; data.last_seen_at = null; data.last_probe_at = null;
  data.checks[0] = { stage: "ssh", outcome: "unavailable", vantage: "control_plane" };
  data.checks[1] = { stage: "configuration", outcome: "not_checked", vantage: "node" };
  data.checks[2] = { stage: "node_http", outcome: "http_ok", vantage: "node", http_status: 200 };
  nodeConnectionApi.diagnose = async () => data;
  const text = en.locale.nodes.connectionDiagnostics;
  show(); await userEvent.setup().click(await screen.findByRole("button", { name: text.run }));
  await screen.findByText(text.outcome.unavailable);
  assert.ok(screen.getAllByText(text.unknown).length >= 3);
  assert.ok(screen.getByText(text.outcome.http_ok, { exact: false }));
  assert.ok(screen.getByText(text.guidance.http_scope));
});

test("switching nodes aborts the previous request and ignores a late report", async () => {
  admin(); let complete!: (value: Report) => void; let signal: AbortSignal | undefined;
  nodeConnectionApi.diagnose = async (uuid, passed) => {
    if (uuid === "node-1") { signal = passed; return new Promise((resolve) => { complete = resolve; }); }
    const next = report(uuid); next.saved_server_url = "https://node-2.example"; return next;
  };
  const text = en.locale.nodes.connectionDiagnostics;
  const view = show(); await userEvent.setup().click(await screen.findByRole("button", { name: text.run }));
  view.rerender(<I18nextProvider i18n={en.i18n}><View nodeUUID="node-2" /></I18nextProvider>);
  await waitFor(() => assert.equal(signal?.aborted, true));
  await userEvent.setup().click(screen.getByRole("button", { name: text.run }));
  await screen.findByText("https://node-2.example");
  complete(report("node-1"));
  await waitFor(() => assert.equal(screen.queryByText("https://saved.example"), null));
});

test("non-administrators cannot invoke SSH diagnostics", async () => {
  api.me = async () => ({ role: "user" }) as never;
  let called = false; nodeConnectionApi.diagnose = async () => { called = true; return report(); };
  const view = show();
  await waitFor(() => assert.equal(view.container.textContent, ""));
  assert.equal(screen.queryByRole("button"), null);
  assert.equal(called, false);
});

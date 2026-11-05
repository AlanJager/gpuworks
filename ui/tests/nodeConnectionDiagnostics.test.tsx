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

function runtimeReport(): Report {
  const value = report();
  value.checks.unshift({ stage: "agent_control", outcome: "runtime_snapshot", vantage: "control_plane", observed_at: "2026-11-05T03:00:01Z" });
  value.agent_report = JSON.parse(`{"report":"unsafe raw report token=secret","host":{"hostname":"unsafe-host"},"findings":[{"code":"bad_token","evidence":[{"name":"unsafe evidence","value":"secret"}]}],"facts":[
    {"code":"connection.source","value":"agent_runtime"},
    {"code":"connection.checked_at","value":"2026-11-05T03:00:00Z"},
    {"code":"connection.server_url","value":"https://running.example/prefix"},
    {"code":"connection.connect_mode","value":"agent_dial"},
    {"code":"connection.server_source","value":"agent_runtime"},
    {"code":"connection.token_present","value":"true"},
    {"code":"connection.control","value":"connected"},
    {"code":"connection.data_lane","value":"pending"},
    {"code":"connection.outcome","value":"ok"},
    {"code":"connection.reason","value":"none"},
    {"code":"connection.phase.dns","value":"not_observed"},
    {"code":"connection.phase.session","value":"failed"},
    {"code":"connection.elapsed_ms.session","value":"1200"},
    {"code":"connection.observed_at.session","value":"2026-11-05T02:59:58Z"},
    {"code":"connection.history_state","value":"unsafe historical marker"},
    {"code":"connection.last_failure.reason","value":"unsafe history reason"}
  ]}`);
  value.last_reject_reason = "proto_mismatch";
  value.last_reject_at = "2026-11-04T03:00:00Z";
  return value;
}

for (const bundle of [en, zh]) {
  test(`runtime evidence stays separate from platform history and local CLI in ${bundle.i18n.language}`, async () => {
    admin(); nodeConnectionApi.diagnose = async () => runtimeReport();
    const text = bundle.locale.nodes.connectionDiagnostics;
    show(bundle);
    const run = await screen.findByRole("button", { name: text.run });
    run.focus(); await userEvent.setup().keyboard("{Enter}");
    await screen.findByText("https://running.example/prefix");
    assert.ok(screen.getByRole("region", { name: text.currentTitle }));
    assert.ok(screen.getByRole("region", { name: text.historyTitle }));
    assert.ok(screen.getByRole("region", { name: text.cliTitle }));
    assert.ok(screen.getByText(text.outcome.runtime_snapshot));
    assert.ok(screen.getByText(text.runtime.value.source.agent_runtime));
    assert.ok(screen.getByText(text.runtime.value.data_lane.pending));
    assert.ok(screen.getByText(text.runtime.phase.not_observed));
    assert.ok(screen.getByText(text.runtime.stage.session));
    assert.ok(screen.getByText(text.runtime.phase.failed));
    assert.ok(screen.getByText(text.cliCommand));
    assert.ok(screen.getByText(text.runtime.scope));
    assert.ok(screen.getByText("https://saved.example"));
    for (const unsafe of ["unsafe raw report", "token=secret", "unsafe-host", "unsafe evidence", "unsafe historical marker", "unsafe history reason", "connection.history_state"]) {
      assert.equal(document.body.textContent?.includes(unsafe), false, unsafe);
    }
  });

  for (const outcome of ["offline", "unsupported", "timeout", "invalid_response"] as const) {
    test(`${outcome} preserves partial history and the CLI guide in ${bundle.i18n.language}`, async () => {
      admin(); const value = report();
      value.checks.unshift({ stage: "agent_control", outcome, vantage: "control_plane" });
      nodeConnectionApi.diagnose = async () => value;
      const text = bundle.locale.nodes.connectionDiagnostics;
      show(bundle); await userEvent.setup().click(await screen.findByRole("button", { name: text.run }));
      await screen.findByText(text.outcome[outcome]);
      assert.ok(screen.getByText("https://saved.example"));
      assert.ok(screen.getByText(text.cliCommand));
      assert.equal(screen.queryByText(text.runtime.value.control.connected), null);
    });
  }
}

test("unknown runtime enums, stages, duplicated facts and unsafe URLs are never rendered as trusted values", async () => {
  admin(); const value = runtimeReport();
  value.agent_report!.facts!.push(
    { code: "connection.control", value: "not_connected" },
    { code: "connection.proxy_url", value: "https://user:unsafe-secret@proxy/?token=unsafe-secret" },
    { code: "connection.phase.untrusted-stage", value: "unsafe-phase" },
    { code: "connection.retry_at", value: "unsafe-time" },
    { code: "connection.proxy", value: "unsafe-proxy" },
    { code: "connection.elapsed_ms.dns", value: "-1" },
  );
  value.agent_report!.facts!.find((item) => item.code === "connection.data_lane")!.value = "none";
  value.checks.push({ stage: "unsafe-stage" as never, outcome: "unsafe-outcome", vantage: "unsafe-location" as never });
  nodeConnectionApi.diagnose = async () => value;
  const text = en.locale.nodes.connectionDiagnostics;
  show(); await userEvent.setup().click(await screen.findByRole("button", { name: text.run }));
  await screen.findByText("https://running.example/prefix");
  for (const unsafe of ["unsafe-secret", "unsafe-phase", "unsafe-time", "unsafe-proxy", "unsafe-stage", "unsafe-outcome", "unsafe-location"]) assert.equal(document.body.textContent?.includes(unsafe), false, unsafe);
  assert.equal(screen.queryByText(text.runtime.value.control.connected), null);
  assert.ok(screen.getAllByText(text.unknown).length > 3);
  assert.equal(document.body.textContent?.includes("-1 ms"), false);
});

test("a response for a different node cannot replace the current node's evidence", async () => {
  admin(); nodeConnectionApi.diagnose = async () => report("unexpected-node");
  const text = en.locale.nodes.connectionDiagnostics;
  show(); await userEvent.setup().click(await screen.findByRole("button", { name: text.run }));
  await screen.findByText(text.failed);
  assert.equal(screen.queryByText("https://saved.example"), null);
});

test("CLI or missing provenance cannot be presented as a running Agent snapshot", async () => {
  admin(); const value = runtimeReport();
  value.agent_report!.facts!.find((item) => item.code === "connection.source")!.value = "cli";
  nodeConnectionApi.diagnose = async () => value;
  const text = en.locale.nodes.connectionDiagnostics;
  show(); await userEvent.setup().click(await screen.findByRole("button", { name: text.run }));
  await screen.findByText("https://saved.example");
  assert.equal(screen.queryByText("https://running.example/prefix"), null);
  assert.equal(screen.queryByText(text.runtime.value.control.connected), null);
});

for (const bundle of [en, zh]) {
  test(`server-initiated identity handshake failure does not imply an outbound connection failure in ${bundle.i18n.language}`, async () => {
    admin(); const value = runtimeReport();
    value.connect_mode = "server_dial";
    value.checks = [
      { stage: "agent_control", outcome: "runtime_snapshot", vantage: "control_plane" },
      { stage: "ssh", outcome: "credentials_unavailable", vantage: "control_plane" },
      { stage: "configuration", outcome: "not_checked", vantage: "node" },
      { stage: "node_http", outcome: "not_applicable", vantage: "node" },
    ];
    value.guidance = ["server_dial_scope"];
    const facts = value.agent_report!.facts!;
    for (const [name, observed] of [["connect_mode", "server_dial"], ["control", "not_connected"], ["data_lane", "not_applicable"], ["outcome", "failed"], ["reason", "transport"]]) {
      facts.find((item) => item.code === `connection.${name}`)!.value = observed;
    }
    value.agent_report!.facts = facts.filter((item) => !item.code.startsWith("connection.phase.") && !item.code.startsWith("connection.elapsed_ms.") && !item.code.startsWith("connection.observed_at."));
    value.agent_report!.facts.push({ code: "connection.phase.hello", value: "failed" });
    const outbound = ["dial", "dns", "connect", "proxy_connect", "tls", "ws_upgrade"] as const;
    for (const stage of outbound) value.agent_report!.facts.push({ code: `connection.phase.${stage}`, value: "not_applicable" });
    nodeConnectionApi.diagnose = async () => value;
    const text = bundle.locale.nodes.connectionDiagnostics;
    show(bundle); await userEvent.setup().click(await screen.findByRole("button", { name: text.run }));
    await screen.findByText(text.runtime.value.control.not_connected);
    assert.ok(screen.getByText(text.runtime.value.connect_mode.server_dial));
    const dataRow = screen.getByText(text.runtime.field.data_lane).closest("tr")!;
    assert.ok(dataRow.textContent?.includes(text.runtime.value.data_lane.not_applicable));
    const heading = screen.getByText(text.runtime.stage.hello);
    assert.equal(heading.textContent, bundle.i18n.language === "en" ? "Identity handshake" : "身份握手");
    const identityStage = heading.closest(".ant-descriptions")!;
    assert.ok(identityStage.textContent?.includes(text.runtime.phase.failed));
    for (const stage of outbound) {
      const section = screen.getByText(text.runtime.stage[stage]).closest(".ant-descriptions")!;
      assert.ok(section.textContent?.includes(text.runtime.phase.not_applicable), stage);
      assert.equal(section.textContent?.includes(text.runtime.phase.failed), false, stage);
    }
    assert.equal(screen.queryByText(text.runtime.value.reason.connect), null);
    assert.equal(screen.queryByText(text.outcome.dns), null);
    assert.equal(screen.queryByText(text.runtime.value.control.smoke_verified), null);
  });
}

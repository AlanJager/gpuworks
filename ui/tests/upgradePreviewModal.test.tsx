import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import type { AgentUpgradePreview } from "../src/agentUpgradesTypes";

const { i18n, locale } = await createI18n(["nodes"]);
const k = locale.nodes.batchUpgrade;
const kind = locale.common.buildInfo;
const { cleanup, render, screen, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: UpgradePreviewModal } = await import("../src/components/agentUpgrades/UpgradePreviewModal.tsx");
const { shortBuild } = await import("../src/buildId.ts");
afterEach(() => cleanup());
after(() => dom.window.close());

function preview(over: Partial<AgentUpgradePreview> = {}): AgentUpgradePreview {
  const nodes = [
    { node_uuid: "a", name: "Offline A", from_version: "v26.11.0", os: "linux", arch: "amd64", transport: "", eligibility: "offline", reason: "node is offline" },
    { node_uuid: "b", name: "Current B", from_version: "v26.12.0", os: "linux", arch: "amd64", transport: "", eligibility: "already_current", reason: "agent already matches the target" },
    { node_uuid: "c", name: "Eligible C", from_version: "v26.11.0", os: "linux", arch: "amd64", transport: "push", eligibility: "eligible", version_change: "upgrade" as const },
  ];
  return { target_version: "v26.12.0", target_build: { version: "v26.12.0", kind: "stable", published_version: "v26.12.0" }, manifest_fingerprint: "sealed", node_uuids: nodes.map((n) => n.node_uuid), nodes, counts: { eligible: 1, offline: 1, already_current: 1 }, ...over };
}
function mount(value: AgentUpgradePreview | null, onStart = () => {}, extra: { loading?: boolean; error?: string } = {}) {
  return render(<I18nextProvider i18n={i18n}><UpgradePreviewModal open loading={extra.loading ?? false} busy={false} error={extra.error ?? ""} preview={value}
    onClose={() => {}} onRefresh={() => {}} onStart={onStart} /></I18nextProvider>);
}

test("the title names the target and its release kind; a stable target needs no further explanation", () => {
  mount(preview());
  const modal = screen.getByRole("dialog", { name: new RegExp(i18n.t("nodes:batchUpgrade.previewTitle", { version: "v26.12.0" })) });
  assert.ok(within(modal).getByText(kind.kind.stable));
  assert.equal(within(modal).queryByText(kind.description.stable), null);
  assert.equal(within(modal).queryByRole("link"), null, "the dialog does not send the user to another page");
});

test("a target that is not a stable release is named by its badge alone", () => {
  mount(preview({ target_version: "dev-x", target_build: { version: "dev-x", kind: "development" } }));
  assert.ok(screen.getByText(kind.kind.development));
  assert.equal(screen.queryByText(kind.description.development), null, "the badge says it; no sentence repeats it");
});

test("columns keep their widths from page to page", () => {
  const many = Array.from({ length: 12 }, (_, i) => ({ node_uuid: `n${i}`, name: `node-${i}`, from_version: i < 10 ? "v0.5.0" : "a-much-longer-build-identifier-0123456789", os: "linux", arch: "amd64", transport: "", eligibility: "offline" }));
  mount(preview({ nodes: many, node_uuids: many.map((n) => n.node_uuid), counts: { offline: 12 } }));
  const table = screen.getByRole("dialog").querySelector("table")!;
  assert.equal(table.style.tableLayout, "fixed");
  const widths = [...table.querySelectorAll("col")].map((col) => col.style.width);
  assert.deepEqual(widths.slice(1), ["320px", "200px"]);
});

test("one line says what will happen, upgradable nodes lead the table and skipped ones show why", async () => {
  let started = 0;
  mount(preview(), () => started++);
  const modal = screen.getByRole("dialog");
  assert.ok(within(modal).getByText(new RegExp(i18n.t("nodes:batchUpgrade.summary", { eligible: 1, skipped: 2 }))));
  assert.ok(within(modal).getByText(new RegExp(k.warning.interrupt.slice(0, 12))));
  const rows = within(modal).getAllByRole("row").slice(1).map((row) => row.textContent ?? "");
  assert.match(rows[0], /Eligible C/);
  assert.match(rows[0], /v26\.11\.0 → v26\.12\.0/);
  assert.ok(within(modal).getByText(k.eligibility.offline));
  assert.ok(within(modal).getByText(k.eligibility.already_current));
  assert.equal(within(modal).queryByRole("checkbox"), null);
  assert.equal(within(modal).queryByText(/node is offline|already matches/), null, "server prose stays out of the localized table");
  await userEvent.setup().click(within(modal).getByRole("button", { name: i18n.t("nodes:batchUpgrade.start", { count: 1 }) }));
  assert.equal(started, 1);
});

test("long build ids keep both ends on one line and their full text on hover", () => {
  const from = "1c9a7b6a-ee.4935a06-agentupg2", to = "b64e92b7-ee.4935a06-agentupg3";
  mount(preview({ target_version: to, nodes: [{ node_uuid: "c", name: "Eligible C", from_version: from, os: "linux", arch: "amd64", transport: "push", eligibility: "eligible", version_change: "different_build" }], node_uuids: ["c"], counts: { eligible: 1 } }));
  const cell = screen.getByText("1c9a7b6a…agentupg2 → b64e92b7…agentupg3");
  assert.equal(cell.getAttribute("title"), `${from} → ${to}`, "the full ids are on hover");
});

test("shortBuild keeps whole segments and leaves short ids alone", () => {
  assert.equal(shortBuild("3c283609-ee.4935a06-gwks522"), "3c283609…gwks522");
  assert.equal(shortBuild("dev-06c711b1"), "dev-06c711b1");
  assert.equal(shortBuild("v0.6.0-rc.1"), "v0.6.0-rc.1");
  assert.equal(shortBuild("0123456789abcdef0123456789"), "0123456789…0123456789");
  assert.equal(shortBuild(undefined), "");
});

test("releases short enough to read are shown whole", () => {
  mount(preview());
  assert.ok(screen.getByText("v26.11.0 → v26.12.0"));
});

test("a fleet blocked by another upgrade keeps the start button but disabled, with the reason on top", () => {
  mount(preview({ blocked_by_item: { run_id: 3, node_uuid: "x", name: "busy-node", reason: "unconfirmed" } }));
  assert.ok(screen.getByText(k.blocked.replace("{{name}}", "busy-node")));
  assert.equal((screen.getByRole("button", { name: i18n.t("nodes:batchUpgrade.start", { count: 1 }) }) as HTMLButtonElement).disabled, true);
});

test("loading and error states render without a preview", () => {
  const view = mount(null, () => {}, { loading: true });
  assert.ok(screen.getByLabelText(k.loadingPreview));
  view.unmount();
  mount(null, () => {}, { error: "artifact unreadable" });
  assert.ok(screen.getByText("artifact unreadable"));
  assert.ok(screen.getByRole("dialog", { name: k.selected }));
});

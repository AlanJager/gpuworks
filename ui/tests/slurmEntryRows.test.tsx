import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { SETUP_COMMAND, SLURM_CATEGORY, KUBERNETES_CATEGORY, KINDS, kindCategories, servePlatforms, slurmEntry } from "./helpers/platformFixtures.ts";
import { rowActions } from "./helpers/rowActions.ts";

// A Slurm cluster connected on its own, as a row of the External Platforms
// page: the quick-connect card, where its entry stands
// (ui/src/pages/slurm/SlurmEntryStatus.tsx), what its row opens to
// (ui/src/pages/platforms/StandaloneSlurmDetail.tsx, SlurmEntryDetail.tsx): the
// facts, how many GPU nodes Slurm lists against how many joined, the nodes
// with no agent and why, and the setup command until a node joins.

const en = await createI18n(["providers", "slurm"], "en");
const zh = await createI18n(["providers", "slurm"], "zh");
const languages = { en: en.i18n, zh: zh.i18n };
const s = en.locale.slurm;
const sZh = zh.locale.slurm;
const { MemoryRouter } = await import("react-router-dom");
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: CloudProvidersPage } = await import("../src/pages/CloudProvidersPage.tsx");
const { api } = await import("../src/api.ts");
const { FeedbackHost } = await import("../src/ui-kit/feedback/index.tsx");

const original = { ...api };
afterEach(() => { cleanup(); Object.assign(api, original); });

function renderPage(address = "/providers", language: keyof typeof languages = "en") {
  const view = render(<I18nextProvider i18n={languages[language]}><FeedbackHost /><MemoryRouter initialEntries={[address]}><CloudProvidersPage /></MemoryRouter></I18nextProvider>);
  return { view, user: userEvent.setup({ document: dom.window.document }) };
}
const rowOf = async (name: string) => (await screen.findByText(name, { selector: "td" })).closest("tr") as HTMLElement;
const statusCell = async (name: string) => within(await rowOf(name)).getAllByRole("cell")[screen.getAllByRole("columnheader").findIndex((header) => ["Status", "状态"].includes(header.textContent ?? ""))];
const facts = (first: string) => {
  const list = screen.getByText(first, { selector: "dt" }).closest("dl") as HTMLElement;
  const terms = within(list).getAllByRole("term").map((term) => term.textContent ?? "");
  const values = within(list).getAllByRole("definition").map((value) => value.textContent ?? "");
  return Object.fromEntries(terms.map((term, index) => [term, values[index]]));
};
const categories = [...kindCategories(KINDS), SLURM_CATEGORY];

const joined = slurmEntry();
const waiting = slurmEntry({ id: 2, uuid: "s-2", name: "lab-new", slurm_cluster_name: "", node_count: 0, slurm_gpu_nodes: 0, missing_count: 0, missing_nodes: [] });
const stalled = slurmEntry({ id: 3, uuid: "s-3", name: "lab-stalled", slurm_cluster_name: "", node_count: 0, slurm_gpu_nodes: 0, missing_count: 0, missing_nodes: [], stalled: true });
const retired = slurmEntry({ id: 4, uuid: "s-4", name: "retired", revoked: true, setup_command: undefined, node_count: 0, slurm_gpu_nodes: 0, missing_count: 0, missing_nodes: [] });

test("the Slurm cluster card is offered to an administrator beside the Kubernetes one, and not to a viewer", async () => {
  servePlatforms(api, { categories: [...kindCategories(KINDS).slice(0, -1), KUBERNETES_CATEGORY, SLURM_CATEGORY] });
  renderPage();
  const card = await screen.findByRole("button", { name: en.locale.providers.page.addKind.replace("{{kind}}", en.locale.providers.page.otherSlurm) });
  // The card has Slurm's own mark, not the initial a kind without a logo falls back to.
  assert.equal(card.querySelector("img.provider-kind-logo")?.getAttribute("src"), "/provider-logos/slurm.svg");
  cleanup();
  servePlatforms(api, { admin: false, categories });
  renderPage();
  await screen.findByRole("button", { name: /Add Alibaba Cloud/ });
  assert.equal(screen.queryAllByRole("button", { name: /Slurm cluster/ }).length, 0, "making an enrollment token takes an administrator");
});

for (const language of ["en", "zh"] as const) {
  test(`in ${language} a Slurm row says where its entry stands: joined, waiting for its first node, waiting too long (with a way to troubleshoot), or revoked`, async () => {
    const copy = language === "en" ? s : sZh;
    servePlatforms(api, { slurm: [joined, waiting, stalled, retired], categories });
    renderPage("/providers", language);
    assert.equal((await statusCell("hpc-lab")).textContent, copy.status.active);
    const pending = await statusCell("lab-new");
    assert.equal(pending.textContent, copy.status.pending);
    assert.equal(within(pending).queryAllByRole("button").length, 0, "the server did not call it stalled");
    const late = await statusCell("lab-stalled");
    assert.ok(within(late).getByText(copy.status.stalled));
    assert.ok(within(late).getByRole("button", { name: copy.status.troubleshoot }));
    assert.equal((await statusCell("retired")).textContent, copy.status.revoked);
  });
}

test("a stalled entry's troubleshooting names the commands for the controller and for a node, in the entry's directories, and the usual causes", async () => {
  servePlatforms(api, { slurm: [slurmEntry({ ...stalled, install_dir: "/data/gw agent" })], categories });
  const { user } = renderPage();
  await user.click(within(await rowOf("lab-stalled")).getByRole("button", { name: s.status.troubleshoot }));
  assert.ok(await screen.findByText(s.stalled.title));
  for (const key of ["notRun", "script", "connect", "conf"] as const) assert.ok(screen.getByText(s.stalled[key]));
  const controller = screen.getByLabelText(s.stalled.on.controller, { selector: "pre" }).textContent ?? "";
  assert.equal(controller, "scontrol show config | grep -i HealthCheck");
  const node = screen.getByLabelText(s.stalled.on.node, { selector: "pre" }).textContent ?? "";
  assert.ok(node.includes("ls -l /opt/gpuworks-slurm/check.sh") && node.includes("tail -n 50 '/data/gw agent/slurm-check.log'"), node);
  await user.click(screen.getByRole("button", { name: s.stalled.copy.replace("{{where}}", s.stalled.on.node) }));
  assert.equal(await navigator.clipboard.readText(), node);
});

test("the row's nodes column counts the joined nodes and links to them by the entry's source", async () => {
  servePlatforms(api, { slurm: [joined], categories });
  renderPage();
  const link = within(await rowOf("hpc-lab")).getByRole("link", { name: "3" });
  assert.equal(link.getAttribute("href"), "/nodes?source=cluster%3As-1");
});

test("an opened Slurm row lays out what the entry is and how many of Slurm's GPU nodes joined; the nodes with no agent are listed with why, as the server says, and nothing is subtracted here", async () => {
  const entry = slurmEntry({
    account_user: "gwks", sbatch_args: "-p gpu -A lab", node_count: 6, online_count: 5, slurm_gpu_nodes: 9, missing_count: 3,
    missing_nodes: [
      { name: "gpu006", reason: "not_enrolled", slurm_state: "drained", slurm_reason: "maintenance" },
      { name: "gpu007", reason: "offline", slurm_state: "idle", node_uuid: "n-7" },
    ],
  });
  servePlatforms(api, { slurm: [entry], categories });
  const { user } = renderPage();
  await user.click(within(await rowOf("hpc-lab")).getByRole("button", { name: "Expand row" }));
  const f = facts(s.fact.connectedBy);
  assert.deepEqual(f, {
    [s.fact.connectedBy]: s.fact.byCommand, [s.fact.clusterName]: "lab", [s.fact.nodes]: "GPU nodes 9, online 5, offline 1",
    [s.fact.serverUrl]: "https://gpuworks.example", [s.fact.runAs]: "root", [s.fact.accountUser]: "gwks",
    [s.fact.installDir]: "/var/lib/gpuworks", [s.fact.scriptDir]: "/opt/gpuworks-slurm", [s.fact.sbatchArgs]: "-p gpu -A lab",
    [s.fact.idleReturn]: "5 min", [s.fact.pendingTimeout]: "10 min", [s.fact.createdAt]: f[s.fact.createdAt],
  });
  const list = screen.getByRole("list", { name: s.missing.list });
  const items = within(list).getAllByRole("listitem").map((item) => item.textContent);
  assert.deepEqual(items, [
    "gpu006never joinedSlurm: drained · maintenance",
    "gpu007joined, agent offlineSlurm: idleRemove",
  ]);
  assert.ok(screen.getByText("3 GPU nodes have no agent online"));
  assert.ok(screen.getByText("3 in all; the first 2 are listed."), "the list is the server's first nodes, the count is of all");
  for (const key of ["script", "connect", "drained"] as const) assert.ok(screen.getByText(s.missing.cause[key]));
  assert.equal(screen.queryAllByText(SETUP_COMMAND).length, 0, "nodes joined: the command is not repeated");
});

test("an entry that no node has joined yet opens to the setup command with what it does and when the nodes appear; a revoked one says its token is revoked and shows no command", async () => {
  servePlatforms(api, { slurm: [waiting, retired], categories });
  const { user } = renderPage();
  await user.click(within(await rowOf("lab-new")).getByRole("button", { name: "Expand row" }));
  assert.ok(await screen.findByText(s.detail.waiting));
  // Measured on a real cluster: the nodes join one to one and a half intervals after the command, not within one.
  assert.match(s.detail.waiting, /within one to one and a half health-check intervals/);
  assert.equal(screen.getByLabelText(s.setup.command, { selector: "pre" }).textContent, SETUP_COMMAND);
  // An entry made by hand has no one to apply it: the commands are its way in, not folded away, and nothing offers to apply.
  assert.equal(screen.queryAllByRole("button", { name: s.detail.manual }).length, 0);
  assert.equal(screen.queryAllByText(s.detail.applyHelp).length, 0);
  for (const key of ["script", "distribute", "conf", "reconfigure"] as const) assert.ok(screen.getByText(s.setup.step[key]));
  // The interval is the server's.
  assert.ok(await screen.findByText("Every GPU node then joins by itself within one to one and a half health-check intervals (an interval is 5 min)."));
  await user.click(screen.getByRole("button", { name: s.command.copy.replace("{{title}}", s.setup.command) }));
  assert.equal(await navigator.clipboard.readText(), SETUP_COMMAND);
  await user.click(within(await rowOf("retired")).getByRole("button", { name: "Expand row" }));
  assert.ok(await screen.findByText(s.detail.revoked));
  assert.equal(screen.getAllByLabelText(s.setup.command, { selector: "pre" }).length, 1, "only the waiting entry's command");
});

test("a standalone entry that waits says why too, as the server's code, above the command it still shows", async () => {
  const entry = slurmEntry({ ...waiting, waiting: { reason: "no_node_called" } });
  servePlatforms(api, { slurm: [entry], categories });
  const { user } = renderPage();
  await user.click(within(await rowOf("lab-new")).getByRole("button", { name: "Expand row" }));
  assert.ok(await screen.findByText(s.waiting.no_node_called.replace("{{url}}", "https://gpuworks.example")));
  assert.equal(screen.queryAllByText(s.detail.waiting).length, 0, "the reason is the one line");
  assert.equal(screen.getByLabelText(s.setup.command, { selector: "pre" }).textContent, SETUP_COMMAND);
});

test("right after the setup the server says the entry is joining: the neutral line, no stalled badge, no cause that blames the nodes", async () => {
  const entry = slurmEntry({ ...waiting, waiting: { reason: "joining" } });
  servePlatforms(api, { slurm: [entry], categories });
  const { user } = renderPage();
  assert.equal((await statusCell("lab-new")).textContent, s.status.pending);
  await user.click(within(await rowOf("lab-new")).getByRole("button", { name: "Expand row" }));
  assert.ok(await screen.findByText(s.waiting.joining));
  assert.equal(screen.queryAllByText(s.waiting.no_node_called.replace("{{url}}", "https://gpuworks.example")).length, 0);
  assert.equal(screen.queryAllByText(s.detail.waiting).length, 0, "the reason is the one line");
});

test("an entry's first report is not here yet: only what joined is said, not a GPU node count of zero", async () => {
  servePlatforms(api, { slurm: [slurmEntry({ slurm_gpu_nodes: 0, missing_count: 0, missing_nodes: [] })], categories });
  const { user } = renderPage();
  await user.click(within(await rowOf("hpc-lab")).getByRole("button", { name: "Expand row" }));
  assert.equal(facts(s.fact.connectedBy)[s.fact.nodes], "3 online");
  assert.equal(screen.queryAllByRole("list", { name: s.missing.list }).length, 0);
});

test("a viewer sees the entries without commands or actions", async () => {
  servePlatforms(api, { slurm: [waiting], admin: false, categories });
  const { user } = renderPage();
  await user.click(await screen.findByRole("button", { name: "Expand row" }));
  assert.equal(screen.queryAllByLabelText(s.setup.command, { selector: "pre" }).length, 0);
  assert.deepEqual(await rowActions(await rowOf("lab-new")), { shown: [], folded: [] });
});

test("an entry made by hand offers Edit and Copy in its row and folds View script, Uninstall, Revoke and Delete into More, the destructive ones last", async () => {
  servePlatforms(api, { slurm: [joined], categories });
  renderPage();
  const actions = await rowActions(await rowOf("hpc-lab"));
  assert.deepEqual(actions.shown, [s.actions.edit, s.actions.copyCommand, en.locale.common.actions.more]);
  assert.deepEqual(actions.folded, [s.actions.script, s.actions.uninstall, s.actions.revoke, s.actions.delete]);
});

test("a failed read of the Slurm entries is said, with a way to retry, and the accounts are still listed", async () => {
  const server = servePlatforms(api, { slurm: new Error("slurm list down"), categories, providers: [] });
  const { user } = renderPage();
  assert.ok(await screen.findByText(en.locale.providers.page.slurmClustersFailed));
  server.slurm = [joined];
  await user.click(screen.getByRole("button", { name: en.locale.providers.instances.retry }));
  assert.ok(await rowOf("hpc-lab"));
});

test("a node's source opens the page on the Slurm row of its entry", async () => {
  servePlatforms(api, { slurm: [joined], categories });
  renderPage("/providers?open=ss-1");
  assert.ok(await screen.findByText(s.fact.connectedBy, { selector: "dt" }), "the row opened");
});

test("an entry whose last agent left says how old its list is, and an administrator removes the offline node it still names; the list is read again", async () => {
  const gone = slurmEntry({
    node_count: 1, online_count: 0, slurm_gpu_nodes: 1, missing_count: 1, list_as_of: "2026-11-18T11:20:00Z",
    missing_nodes: [{ name: "compute001", reason: "offline", slurm_state: "idle", node_uuid: "n-1" }],
  });
  const server = servePlatforms(api, { slurm: [gone], categories });
  const removed: Array<[string, boolean]> = [];
  Object.assign(api, {
    deleteNode: async (uuid: string, force = false) => {
      removed.push([uuid, force]);
      server.slurm = [slurmEntry({ node_count: 0, online_count: 0, slurm_gpu_nodes: 0, missing_count: 0, missing_nodes: [] })];
      return { status: "deleted", agent_uninstalled: true };
    },
  });
  const { user } = renderPage();
  await user.click(within(await rowOf("hpc-lab")).getByRole("button", { name: "Expand row" }));
  assert.match(facts(s.fact.connectedBy)[s.fact.nodes], /^GPU nodes 1 \(list as of .+\), online 0, offline 1$/);
  await user.click(screen.getByRole("button", { name: s.missing.remove }));
  assert.ok(await screen.findByText("Remove node compute001?"));
  assert.ok(screen.getByText(s.missing.removeHelp));
  await user.click(screen.getAllByRole("button", { name: s.missing.remove }).at(-1)!);
  await waitFor(() => assert.equal(screen.queryAllByRole("list", { name: s.missing.list }).length, 0, "the list is read again after the removal"));
  assert.deepEqual(removed, [["n-1", false]]);
});

test("a viewer is not offered to remove an offline node", async () => {
  servePlatforms(api, { slurm: [slurmEntry({ online_count: 2, missing_nodes: [{ name: "gpu003", reason: "offline", slurm_state: "idle", node_uuid: "n-3" }] })], admin: false, categories });
  const { user } = renderPage();
  await user.click(await screen.findByRole("button", { name: "Expand row" }));
  assert.equal(screen.queryAllByRole("button", { name: s.missing.remove }).length, 0);
});

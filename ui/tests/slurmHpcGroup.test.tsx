import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { account, aliyunKind, CHECK_COMMAND, ehpcEntry, hpcCluster, hpcClusterList, SETUP_COMMAND } from "./helpers/platformFixtures.ts";
import { clusterHeader, facts, groupHeader, groupOf, groupRows } from "./helpers/platformDetail.ts";
import { list } from "./helpers/listPage.ts";

// An E-HPC cluster of a cloud account as a group with its Slurm entry
// (ui/src/pages/platforms/HpcClusterGroup.tsx), laid out like a managed
// Kubernetes cluster's: the cloud's live listing gives its facts, Connect opens
// the entry form prefilled from it and only makes the entry; setting the
// cluster up is a separate step that checks first and applies on the
// operator's confirmation (SlurmApplyModal.tsx, SlurmCheckPlanView.tsx,
// SlurmApplyOutcomeView.tsx); once there is an entry the header says where it
// stands, with its actions.

const en = await createI18n(["providers", "slurm"], "en");
const zh = await createI18n(["providers", "slurm"], "zh");
const s = en.locale.slurm;
const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: ProviderResources } = await import("../src/pages/platforms/ProviderResources.tsx");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { FeedbackHost } = await import("../src/ui-kit/feedback/index.tsx");
import type { ProviderHPCCluster, ProviderHPCClusterList } from "../src/api";
import type { SlurmApplyCheck, SlurmApplyOutcome, SlurmCluster, SlurmClusterRequest, SlurmDefaults, SlurmReach } from "../src/slurmApi";
import { SLURM_DEFAULTS } from "./helpers/platformFixtures.ts";

const original = { ...api };
(globalThis as unknown as { CSS?: unknown }).CSS ??= { escape: (value: string) => value.replace(/[^\w-]/g, (char) => `\\${char}`) };
afterEach(() => { cleanup(); Object.assign(api, original); });

const p = en.locale.providers;
const ali = account(3, "ali-prod", "aliyun", { instance_count: 2 });
const gpuLab = hpcCluster("ehpc-sz-1", "gpu-lab");

const gres = { added: ["gpu000", "gpu001"], present: ["gpu002"], no_gpu_gres: [] };
const outcome = (over: Partial<SlurmApplyOutcome> = {}): SlurmApplyOutcome => ({
  status: "ok", manager_instance: "i-mgr",
  steps: [{ id: "gres", status: "pass" }, { id: "check_script", status: "pass" }, { id: "distribute", status: "skip" }, { id: "slurm_conf", status: "pass" }, { id: "reconfigure", status: "pass" }, { id: "resume", status: "pass" }],
  gres, distribute: { shared: true, ok: 0, failed: [] },
  resumed_nodes: ["gpu000"], resume_failed_nodes: [], reach: { hosts: [] }, cluster: ehpcEntry(), ...over,
});
const plan = (over: Partial<SlurmApplyCheck> = {}): SlurmApplyCheck => ({
  dry_run: true, status: "ok", manager_instance: "i-mgr", slurm_cluster: "demo",
  gres: { added: [], present: ["gpu002"], no_gpu_gres: [], would_add: [
    { node: "gpu000", files: "/dev/nvidia0", line: "NodeName=gpu000 Name=gpu File=/dev/nvidia0" }, { node: "gpu001", files: "/dev/nvidia[0-3]", line: "NodeName=gpu001 Name=gpu File=/dev/nvidia[0-3]" }] },
  script: { check_script: "/opt/gpuworks-slurm/check.sh", token_file: "/opt/gpuworks-slurm/token", shared: false, fstype: "ext4", copy_to: ["gpu000", "gpu001"], copy_files: ["check.sh", "token", "chained"] },
  conf: { path: "/etc/slurm/slurm.conf", changed: true, interval: 60, node_state: "ANY,CYCLE",
    block: ["HealthCheckProgram=/opt/gpuworks-slurm/check.sh", "HealthCheckInterval=60", "HealthCheckNodeState=ANY,CYCLE"],
    chained_program: "/usr/sbin/nhc", chained_state: "IDLE,CYCLE", commented: [{ line: 7, key: "HealthCheckProgram", value: "/usr/sbin/nhc" }], includes: [] },
  configless: true, rearm: false, resume_nodes: ["gpu000"], warnings: [], reach: { hosts: [] }, cluster: ehpcEntry(), ...over,
});

type Calls = Record<string, unknown[][]>;
type Options = {
  clusters?: unknown[] | Error;
  /** What the server says besides the clusters (access, partial, failed_regions). */
  listing?: Partial<ProviderHPCClusterList>;
  check?: (n: number) => SlurmApplyCheck | Error;
  apply?: (n: number) => SlurmApplyOutcome | Error;
  create?: Error | ((body: SlurmClusterRequest) => SlurmCluster);
  /** What GET /slurm/defaults says besides the usual. */
  defaults?: Partial<SlurmDefaults>;
};
function stub(options: Options = {}) {
  const calls: Calls = {};
  Object.assign(api, {
    providerInstances: async () => list([]),
    providerClusters: async () => list([]),
    providerHPCClusters: async (...args: unknown[]) => {
      (calls.hpc ??= []).push(args);
      if (options.clusters instanceof Error) throw options.clusters;
      return hpcClusterList((options.clusters ?? [gpuLab]) as ProviderHPCCluster[], options.listing);
    },
    slurmDefaults: async () => ({ ...SLURM_DEFAULTS, ...options.defaults }),
    createSlurmCluster: async (...args: unknown[]) => {
      (calls.create ??= []).push(args);
      if (options.create instanceof Error) throw options.create;
      return options.create ? options.create(args[0] as SlurmClusterRequest) : ehpcEntry({ name: (args[0] as SlurmClusterRequest).name });
    },
    checkSlurmCluster: async (...args: unknown[]) => {
      (calls.check ??= []).push(args);
      const answer = options.check ? options.check(calls.check.length) : plan();
      if (answer instanceof Error) throw answer;
      return answer;
    },
    applySlurmCluster: async (...args: unknown[]) => {
      (calls.apply ??= []).push(args);
      const answer = options.apply ? options.apply(calls.apply.length) : outcome();
      if (answer instanceof Error) throw answer;
      return answer;
    },
  });
  return calls;
}

function mount(entries: SlurmCluster[] = [], language: "en" | "zh" = "en", props: { refreshKey?: string; focusCluster?: string } = {}) {
  const changes = { count: 0 };
  const tree = (refreshKey: string) => (
    <I18nextProvider i18n={(language === "en" ? en : zh).i18n}><FeedbackHost /><MemoryRouter>
      <ProviderResources provider={ali} kindInfo={aliyunKind} refreshKey={refreshKey} entries={{}} slurmEntries={entries} focusCluster={props.focusCluster} onEntriesChanged={() => { changes.count += 1; }} admin />
    </MemoryRouter></I18nextProvider>
  );
  const view = render(tree(props.refreshKey ?? "1"));
  return { changes, user: userEvent.setup({ document: dom.window.document }), sync: (key: string) => view.rerender(tree(key)) };
}
const dialog = async () => within(await screen.findByRole("dialog"));
const noDialog = () => waitFor(() => assert.equal(screen.queryAllByRole("dialog").length, 0, "the dialog closed"));
const escape = () => fireEvent.keyDown(screen.queryAllByRole("dialog").at(-1) ?? document.activeElement ?? document.body, { key: "Escape", code: "Escape", keyCode: 27 });
const connect = async (user: ReturnType<typeof userEvent.setup>, name = "gpu-lab", label = s.hpc.connect) => {
  const group = await groupOf(name);
  await user.click(within(await groupHeader(group)).getByRole("button", { name: label }));
  return dialog();
};
// Makes the entry from the open form, and opens the operation from the result.
const createThenApply = async (user: ReturnType<typeof userEvent.setup>) => {
  const d = await connect(user);
  await user.click(d.getByRole("button", { name: s.form.submit }));
  await user.click(await d.findByRole("button", { name: s.created.applyNow }));
  const modal = within((await screen.findAllByRole("dialog")).at(-1) as HTMLElement);
  return { d, modal };
};
const stepsShown = (d: ReturnType<typeof within>) => within(d.getByRole("list", { name: s.apply.steps }) as HTMLElement).getAllByRole("listitem").map((item) => item.textContent);

test("an account's E-HPC cluster is a group built from the cloud's live listing, with its facts and no node rows or per-node buttons", async () => {
  const calls = stub();
  mount();
  const group = await groupOf("gpu-lab");
  assert.deepEqual(calls.hpc, [[3, undefined]], "the listing is read once, for the account, not fresh");
  const header = await clusterHeader(group);
  assert.deepEqual(header.title, ["gpu-lab", "Slurm 22.05.8"]);
  assert.equal(header.standing, s.status.pending);
  assert.deepEqual(await facts(group), {
    [p.clusters.field.id]: "ehpc-sz-1", [p.clusters.field.region]: "cn-shenzhen", [p.hpc.field.scheduler]: "Slurm 22.05.8", [p.hpc.field.gpuNodes]: "2",
  });
  assert.equal((await groupRows(group)).length, 1, "the facts are one row; the cluster's nodes are not listed");
  for (const name of [/SSH/i, /Cloud Assistant/i, /Install/i, /Power/i]) assert.equal(within(group).queryAllByRole("button", { name }).length, 0, String(name));
  assert.ok(within(await groupHeader(group)).getByRole("button", { name: s.hpc.connect }));
  assert.equal(document.body.textContent?.includes("cannot be powered here"), false);
});

test("the cloud's live count of compute nodes is shown under its own name, and the synced GPU count only when the cloud gives none", async () => {
  stub({ clusters: [
    hpcCluster("ehpc-sz-1", "grown", { compute_nodes: 3, gpu_nodes: undefined }),
    hpcCluster("ehpc-sz-9", "emptied", { compute_nodes: 0, gpu_nodes: undefined }),
    hpcCluster("ehpc-sz-4", "no-count", { gpu_nodes: 2 }),
  ] });
  mount();
  const grown = await facts(await groupOf("grown"));
  assert.equal(grown[p.hpc.field.computeNodes], "3");
  assert.equal(grown[p.hpc.field.gpuNodes], undefined, "the cloud does not say which have a GPU");
  assert.equal((await facts(await groupOf("emptied")))[p.hpc.field.computeNodes], "0", "zero is a count");
  const fallback = await facts(await groupOf("no-count"));
  assert.equal(fallback[p.hpc.field.gpuNodes], "2");
  assert.equal(fallback[p.hpc.field.computeNodes], undefined);
});

test("a cluster with no GPU node yet, before any sync, is listed and can be connected", async () => {
  stub({ clusters: [hpcCluster("ehpc-sz-9", "fresh", { gpu_nodes: undefined })] });
  mount();
  const group = await groupOf("fresh");
  // No sync has listed it: the cloud count is unknown, not shown as 0.
  assert.equal((await facts(group))[p.hpc.field.gpuNodes], undefined);
  assert.ok(within(await groupHeader(group)).getByRole("button", { name: s.hpc.connect }));
});

test("a cluster that does not run Slurm says so and offers no Connect; one that is not running says its state", async () => {
  stub({ clusters: [hpcCluster("ehpc-sz-2", "pbs-lab", { scheduler: "pbs", scheduler_version: "19.1.1" }), hpcCluster("ehpc-sz-3", "booting", { state: "creating", running: false })] });
  mount();
  const pbs = await groupOf("pbs-lab");
  assert.ok(within(pbs).getByText(p.hpc.unsupported.replace("{{scheduler}}", "PBS")));
  assert.equal(within(await groupHeader(pbs)).queryAllByRole("button", { name: s.hpc.connect }).length, 0);
  const booting = await groupOf("booting");
  assert.equal((await clusterHeader(booting)).standing, p.hpc.state.creating);
  assert.ok(within(booting).getByText(p.hpc.notConnectable.cluster_starting));
  assert.equal(within(await groupHeader(booting)).queryAllByRole("button", { name: s.hpc.connect }).length, 0);
});

test("several clusters in different states each say their state once, in words, and why they cannot be connected yet", async () => {
  stub({ clusters: [
    hpcCluster("ehpc-1", "ready"),
    hpcCluster("ehpc-2", "booting", { state: "initing", running: false }),
    hpcCluster("ehpc-3", "broken", { state: "exception", running: false }),
    hpcCluster("ehpc-4", "paused", { state: "stopped", running: false }),
    hpcCluster("ehpc-5", "going", { state: "releasing", running: false }),
    hpcCluster("ehpc-6", "waiting", { state: "pending", running: false }),
    // The cloud has not named its scheduler yet: its state is the reason, not "unknown scheduler".
    hpcCluster("ehpc-7", "new", { state: "creating", running: false, scheduler: "", scheduler_version: "" }),
    hpcCluster("ehpc-8", "odd", { state: "unknown", vendor_state: "Upgrading", running: false }),
    hpcCluster("ehpc-9", "pbs-lab", { scheduler: "pbs", scheduler_version: "19.1.1" }),
  ] });
  mount();
  const expected: Array<[string, string, string | null]> = [
    ["ready", s.status.pending, null],
    ["booting", p.hpc.state.initing, p.hpc.notConnectable.cluster_starting],
    ["broken", p.hpc.state.exception, p.hpc.notConnectable.cluster_exception],
    ["paused", p.hpc.state.stopped, p.hpc.notConnectable.cluster_stopped],
    ["going", p.hpc.state.releasing, p.hpc.notConnectable.cluster_releasing],
    ["waiting", p.hpc.state.pending, p.hpc.notConnectable.cluster_pending],
    ["new", p.hpc.state.creating, p.hpc.notConnectable.cluster_starting],
    ["odd", p.hpc.state.unknown.replace("{{state}}", "Upgrading"), p.hpc.notConnectable.cluster_state_unknown],
    ["pbs-lab", p.hpc.state.running, p.hpc.unsupported.replace("{{scheduler}}", "PBS")],
  ];
  for (const [name, standing, note] of expected) {
    const group = await groupOf(name);
    assert.equal((await clusterHeader(group)).standing, standing, `${name}: the header says the state`);
    assert.equal(within(group).queryAllByText(standing).length, 1, `${name}: the state is said once, not again among the facts`);
    assert.equal(Object.keys(await facts(group)).includes("Status"), false, `${name}: no raw state fact`);
    if (note) assert.ok(within(group).getByText(note), `${name}: ${note}`);
    // Raw cloud words never reach the page for a state the server knows.
    for (const raw of ["initing", "exception", "releasing", "creating"]) assert.equal(within(group).queryAllByText(raw).length, 0, `${name}: raw ${raw}`);
  }
  // A cluster still being created does not claim a scheduler that is not Slurm.
  assert.equal(within(await groupOf("new")).queryAllByText(/Only Slurm clusters/).length, 0);
});

test("a cluster whose state is not running shows it next to its entry's own state", async () => {
  stub({ clusters: [hpcCluster("ehpc-3", "broken", { state: "exception", running: false, entry: { uuid: "e-1", name: "broken" } })] });
  mount([ehpcEntry({ uuid: "e-1", name: "broken", source: { provider_id: 3, provider_name: "ali-prod", provider_kind: "aliyun", provider_cluster_id: "ehpc-3" } })]);
  const group = await groupOf("broken");
  const { standing } = await clusterHeader(group);
  assert.ok(standing.includes(p.hpc.state.exception), standing);
  assert.equal(within(group).queryAllByText(p.hpc.state.exception).length, 1);
});

test("Connect opens the entry form prefilled from the cluster; its only submit makes the entry, and applying is not part of it", async () => {
  const calls = stub();
  const { user } = mount();
  const d = await connect(user);
  assert.ok(d.getByText(s.form.fromSource.replace("{{source}}", "ali-prod · gpu-lab")));
  assert.equal((d.getByLabelText(/^Name/) as HTMLInputElement).value, "gpu-lab");
  assert.equal((d.getByLabelText(/^Management node address/) as HTMLInputElement).value, "http://localhost");
  await user.click(d.getByText(s.form.advanced));
  await waitFor(() => assert.equal((d.getByLabelText(s.form.scriptDir) as HTMLInputElement).placeholder, "/opt/gpuworks-slurm"));
  const names = [...screen.getByRole("dialog").querySelectorAll("button")].filter((b) => !b.hasAttribute("aria-label") && !b.hidden).map((b) => b.textContent);
  assert.deepEqual(names.slice(-2), [s.form.cancel, s.form.submit]);
  await user.click(d.getByRole("button", { name: s.form.submit }));
  assert.ok(await d.findByText(s.created.title.replace("{{name}}", "gpu-lab")));
  assert.deepEqual(calls.create[0][0], { name: "gpu-lab", server_url: "http://localhost", provider_id: 3, provider_cluster_id: "ehpc-sz-1" });
  assert.equal(calls.check, undefined, "making the entry checked nothing");
  assert.equal(calls.apply, undefined, "making the entry applied nothing");
  assert.equal(d.getByLabelText(s.setup.command, { selector: "pre" }).textContent, SETUP_COMMAND);
  assert.equal(d.getByLabelText(s.setup.checkCommand, { selector: "pre" }).textContent, CHECK_COMMAND, "the command that only checks is offered to an administrator who runs it by hand");
  assert.ok(d.getByRole("button", { name: s.created.applyNow }));
});

test("Check and apply checks first and shows the plan: the gres.conf lines, how the script reaches the nodes, the slurm.conf block, what is commented and chained, the nodes resumed; nothing is applied until Apply", async () => {
  const calls = stub();
  const { user } = mount();
  const { modal } = await createThenApply(user);
  assert.ok(await modal.findByText(s.operation.checked));
  assert.deepEqual(calls.check, [["s-7"]]);
  assert.equal(calls.apply, undefined);
  assert.deepEqual(within(modal.getByText(s.plan.gres.add).nextElementSibling as HTMLElement).getAllByRole("listitem").map((item) => item.textContent),
    ["NodeName=gpu000 Name=gpu File=/dev/nvidia0", "NodeName=gpu001 Name=gpu File=/dev/nvidia[0-3]"]);
  assert.equal(modal.getByText(s.plan.gres.present).nextElementSibling?.textContent, "gpu002");
  assert.ok(modal.getByText("Copies the files (check.sh, token, chained) to 2 nodes: gpu000, gpu001."));
  assert.equal(modal.getByLabelText(s.plan.conf.block, { selector: "pre" }).textContent, "HealthCheckProgram=/opt/gpuworks-slurm/check.sh\nHealthCheckInterval=60\nHealthCheckNodeState=ANY,CYCLE");
  assert.equal(modal.getByText(s.plan.conf.commented).nextElementSibling?.textContent, "line 7: HealthCheckProgram=/usr/sbin/nhc");
  assert.ok(modal.getByText("The cluster's own health check /usr/sbin/nhc keeps running after ours, in the node states IDLE,CYCLE."));
  assert.equal(modal.getByText(s.plan.resume).nextElementSibling?.textContent, "gpu000");
  assert.equal(modal.queryAllByText(s.plan.attention).length, 0);
  await user.click(modal.getByRole("button", { name: s.operation.confirm }));
  assert.ok(await modal.findByText(s.apply.done));
  assert.deepEqual(calls.apply, [["s-7"]]);
  assert.equal(calls.create.length, 1);
  assert.deepEqual(stepsShown(modal), [
    "Donegres.conf for the compute nodes", "DoneWrite the check script", "Nothing to doReach the compute nodes with the script",
    "DoneAdd the health check to slurm.conf", "DoneReload Slurm's configuration", "DoneResume the held nodes",
  ]);
  assert.equal(modal.getByText(s.apply.gresAdded).nextElementSibling?.textContent, "gpu000, gpu001");
});

test("a plan with points that need attention says each, by the server's code, before anything is applied", async () => {
  stub({ check: () => plan({
    status: "partial", warnings: ["conf_include", "program_not_in_conf", "not_configless", "no_gpu_gres"],
    gres: { added: [], present: [], would_add: [], no_gpu_gres: ["gpu006"] },
    script: { check_script: "/opt/gpuworks-slurm/check.sh", token_file: "/opt/gpuworks-slurm/token", shared: true, fstype: "nfs4", copy_to: [], copy_files: ["check.sh", "token"] },
    conf: { path: "/etc/slurm/slurm.conf", changed: false, interval: 300, node_state: "ANY", block: [], commented: [], includes: ["/etc/slurm/a.conf", "/etc/slurm/b.conf"], unnamed_program: "/usr/sbin/nhc" },
    configless: false, rearm: true, resume_nodes: [],
  }) });
  const { user } = mount();
  const { modal } = await createThenApply(user);
  assert.ok(await modal.findByText(s.operation.checkedPartial));
  const warnings = within(modal.getByText(s.plan.attention).parentElement as HTMLElement).getAllByRole("listitem").map((item) => item.textContent);
  assert.deepEqual(warnings, [
    "slurm.conf includes /etc/slurm/a.conf, /etc/slurm/b.conf: health check settings in them are not seen by the edit, and the result is verified afterwards.",
    "slurmctld runs the health check program /usr/sbin/nhc, which slurm.conf does not name (an included file, or an edit not yet reloaded): it is chained, but a setting in an included file may override ours.",
    "This cluster is not configless: copy /etc/slurm/slurm.conf to the compute nodes yourself after applying, or the health check does not take effect there.",
    "gpu006 have no gpu Gres in slurm.conf, so nothing is added to gres.conf for them: add Gres=gpu:<count> to their NodeName lines first.",
  ]);
  assert.ok(modal.getByText(s.plan.gres.none));
  assert.ok(modal.getByText("The script directory is on shared storage (nfs4): nothing is copied."));
  assert.ok(modal.getByText(s.plan.conf.unchanged));
  assert.ok(modal.getByText(s.plan.rearm));
  assert.equal((modal.getByRole("button", { name: s.operation.confirm }) as HTMLButtonElement).disabled, false, "the operator decides");
});

test("Cancel on the plan closes the dialog and nothing was applied", async () => {
  const calls = stub();
  const { user } = mount();
  const { modal } = await createThenApply(user);
  await modal.findByText(s.operation.checked);
  await user.click(modal.getByRole("button", { name: s.operation.cancel }));
  await waitFor(() => assert.equal(screen.queryAllByRole("heading", { name: s.operation.title.replace("{{name}}", "gpu-lab") }).length, 0));
  assert.equal(calls.apply, undefined);
});

test("a setup that finished in part says what is left to do, by node", async () => {
  stub({ apply: () => outcome({
    status: "partial",
    steps: [{ id: "gres", status: "warn" }, { id: "check_script", status: "pass" }, { id: "distribute", status: "warn" }, { id: "slurm_conf", status: "pass" }, { id: "reconfigure", status: "pass" }, { id: "resume", status: "warn" }],
    gres: { added: ["gpu000"], present: [], no_gpu_gres: ["gpu006"] }, distribute: { shared: false, ok: 2, failed: ["gpu003", "gpu004"] },
    resumed_nodes: [], resume_failed_nodes: ["gpu001"],
  }) });
  const { user } = mount();
  const { modal } = await createThenApply(user);
  await user.click(await modal.findByRole("button", { name: s.operation.confirm }));
  assert.ok(await modal.findByText(s.apply.partial));
  const todo = within(modal.getByText(s.apply.todoTitle).nextElementSibling as HTMLElement).getAllByRole("listitem").map((item) => item.textContent);
  assert.deepEqual(todo, [
    "The check script could not be copied to gpu003, gpu004: copy it to the same path on each of them.",
    "gpu006 have no gpu Gres in slurm.conf, so nothing was added to gres.conf: add Gres=gpu:<count> to their NodeName lines first.",
    "Some nodes could not be resumed (gpu001): check them with sinfo.",
  ]);
  assert.equal(modal.getByText(s.apply.distributeFailed).nextElementSibling?.textContent, "gpu003, gpu004");
  assert.equal(stepsShown(modal)[0], "Needs attentiongres.conf for the compute nodes");
});

const refusals = [
  ["a step that failed with the reason the script gave, and what the manager node printed", new APIError("SLURM_APPLY_FAILED", { step: "setup", reason: "not_root", output: "line one\nline two" }, "x", 424), "The setup on the Slurm manager node did not finish (setting up the health check): the script did not run as root"],
  ["a reason the console has no text for falls back to the step", new APIError("SLURM_APPLY_FAILED", { step: "assistant", reason: "brand_new", output: "" }, "x", 424), "The setup on the Slurm manager node did not finish (reaching the Slurm manager node through the cloud's command channel): reaching the Slurm manager node through the cloud's command channel"],
  ["a cloud key that lacks a permission, naming it", new APIError("SLURM_APPLY_PERMISSION_MISSING", { permission: "ecs:RunCommand" }, "x", 424), "The cloud account's key may not call ecs:RunCommand. Allow it in the key's RAM policy and try again, or run the setup command on the Slurm manager node by hand."],
  ["a manager node the cloud did not name", new APIError("SLURM_APPLY_MANAGER_UNKNOWN", {}, "x", 424), "The cloud did not say which instance is the cluster's Slurm manager node, so the platform has nowhere to run the setup. Run the setup command on the Slurm manager node by hand."],
  ["a cluster the cloud's listing does not have", new APIError("SLURM_APPLY_NOT_APPLICABLE", { reason: "cluster_unknown" }, "x", 409), "The platform cannot set this cluster up itself: the last sync listed no node of the cluster and the cloud's listing does not have it: check the account or sync it."],
] as const;
for (const [name, failure, text] of refusals) {
  test(`a refused check is a coded error in the operator's language: ${name}; Apply stays off and the check can be run again`, async () => {
    const calls = stub({ check: (n) => n === 1 ? failure : plan() });
    const { user } = mount();
    const { modal } = await createThenApply(user);
    assert.ok(await modal.findByText(text));
    if (failure.params?.output) assert.equal(modal.getByLabelText(s.apply.output, { selector: "pre" }).textContent, "line one\nline two");
    assert.equal(modal.queryAllByRole("button", { name: s.operation.confirm }).length, 0, "there is no plan to confirm");
    assert.equal(calls.apply, undefined);
    await user.click(modal.getByRole("button", { name: s.operation.checkAgain }));
    assert.ok(await modal.findByText(s.operation.checked));
    assert.equal(calls.check.length, 2);
    assert.equal(calls.create.length, 1, "the entry is not made twice");
  });
}

test("a refused apply after a good check is said the same way and can be tried again without checking twice", async () => {
  const calls = stub({ apply: (n) => n === 1 ? new APIError("SLURM_APPLY_FAILED", { step: "run", reason: "busy", output: "another apply is running" }, "x", 424) : outcome() });
  const { user } = mount();
  const { modal } = await createThenApply(user);
  await user.click(await modal.findByRole("button", { name: s.operation.confirm }));
  assert.ok(await modal.findByText(/did not finish/));
  assert.equal(modal.getByLabelText(s.apply.output, { selector: "pre" }).textContent, "another apply is running");
  await user.click(modal.getByRole("button", { name: s.apply.retry }));
  assert.ok(await modal.findByText(s.apply.done));
  assert.equal(calls.apply.length, 2);
  assert.equal(calls.check.length, 1);
});

test("a cluster the platform cannot run commands on is connected by creating the entry and running its command by hand, and the server's reason is said", async () => {
  const calls = stub({ clusters: [hpcCluster("ehpc-sz-1", "gpu-lab", { can_apply: false })],
    create: (body) => ehpcEntry({ name: body.name, can_apply: false, can_remove: false, operation_blocked: "provider_unsupported" }) });
  const { user } = mount();
  const d = await connect(user);
  await user.click(d.getByRole("button", { name: s.form.submit }));
  assert.ok(await d.findByText(s.created.applyBlocked));
  assert.ok(d.getByText(s.blocked.provider_unsupported));
  assert.equal(d.getByLabelText(s.setup.command, { selector: "pre" }).textContent, SETUP_COMMAND);
  assert.equal(d.queryAllByRole("button", { name: s.created.applyNow }).length, 0);
  assert.equal(calls.apply, undefined);
});

test("an entry the cluster already has is refused by the server, and said on the form without losing what was typed", async () => {
  stub({ create: new APIError("SLURM_CLUSTER_SOURCE_TAKEN", { cluster: "ehpc-sz-1" }, "x", 409) });
  const { user } = mount();
  const d = await connect(user);
  await user.click(d.getByRole("button", { name: s.form.submit }));
  assert.ok(await d.findByText("An entry for the cloud cluster ehpc-sz-1 already exists."));
  assert.equal((d.getByLabelText(/^Name/) as HTMLInputElement).value, "gpu-lab");
});

test("Enter in the name field makes the entry, and Escape closes the drawer before anything was made", async () => {
  const calls = stub();
  const { user } = mount();
  let d = await connect(user);
  escape();
  await noDialog();
  assert.equal(calls.create, undefined);
  d = await connect(user);
  await user.click(d.getByLabelText(/^Name/));
  await user.keyboard("{Enter}");
  assert.ok(await d.findByText(s.created.title.replace("{{name}}", "gpu-lab")));
  assert.equal(calls.create.length, 1);
  assert.equal(calls.apply, undefined);
});

test("with an entry, the cluster's group says where it stands and how many GPU nodes joined, offers the entry's actions instead of Connect, and lists the nodes with no agent, never the cluster's nodes one by one", async () => {
  stub({ clusters: [hpcCluster("ehpc-sz-1", "gpu-lab", { entry: { uuid: "s-7", name: "gpu-lab" } })] });
  const entry = ehpcEntry({ name: "gpu-lab", slurm_cluster_name: "gpu-lab", node_count: 1, online_count: 1, slurm_gpu_nodes: 3, missing_count: 2,
    missing_nodes: [{ name: "gpu001", reason: "offline", slurm_state: "idle" }, { name: "gpu002", reason: "not_enrolled", slurm_state: "idle" }] });
  mount([entry, ehpcEntry({ id: 8, uuid: "s-8", name: "other", source: { provider_id: 4, provider_name: "x", provider_kind: "aliyun", provider_cluster_id: "ehpc-sz-1" } })]);
  const group = await groupOf("gpu-lab");
  const header = within(await groupHeader(group));
  assert.ok(header.getByText(s.status.active));
  assert.ok(header.getByText("GPU nodes 3, online 1"));
  assert.equal(header.queryAllByRole("button", { name: s.hpc.connect }).length, 0, "one entry per cluster");
  assert.ok(header.getByRole("button", { name: s.actions.edit }));
  assert.ok(header.getByRole("button", { name: s.actions.apply }));
  const f = await facts(group);
  assert.equal(f[p.clusters.field.id], "ehpc-sz-1");
  // The entry's counts (what the agents report) replace the cloud's synced count, so the group never shows two numbers.
  assert.equal(f[p.hpc.field.gpuNodes], undefined);
  assert.equal(f[s.fact.nodes], "GPU nodes 3, online 1");
  const items = within(group).getAllByRole("listitem").map((item) => item.textContent);
  assert.deepEqual(items.slice(0, 2), ["gpu001joined, agent offlineSlurm: idle", "gpu002never joinedSlurm: idle"]);
});

test("an entry whose cluster is gone from the account is still listed, so it can be uninstalled, revoked or deleted", async () => {
  stub({ clusters: [] });
  mount([ehpcEntry({ name: "old-cluster", node_count: 2, slurm_gpu_nodes: 2, slurm_cluster_name: "old" })]);
  const group = await groupOf("old-cluster");
  assert.ok(within(group).getByText(p.hpc.entryGone));
  assert.equal(within(group).queryAllByText(p.hpc.empty).length, 0, "an entry is something, so the account does not read as empty");
  const header = within(await groupHeader(group));
  assert.ok(header.getByRole("button", { name: s.actions.edit }));
  assert.equal(header.queryAllByRole("button", { name: s.hpc.connect }).length, 0);
  assert.equal((await facts(group))[p.clusters.field.id], "ehpc-sz-1");
});

test("when the cloud's listing failed, an entry says the cluster could not be read, not that it is gone, and the failure has a retry", async () => {
  const calls = stub({ clusters: new Error("list E-HPC clusters: region cn-beijing: boom") });
  const { user } = mount([ehpcEntry({ name: "kept" })]);
  const group = await groupOf("kept");
  assert.ok(within(group).getByText(p.hpc.entryUnread));
  assert.equal(within(group).queryAllByText(p.hpc.entryGone).length, 0);
  const alert = await screen.findByText(p.hpc.loadFailed);
  assert.ok(within(alert.parentElement as HTMLElement).getByText(/region cn-beijing: boom/));
  Object.assign(api, { providerHPCClusters: async (...args: unknown[]) => { (calls.hpc ??= []).push(args); return hpcClusterList([gpuLab]); } });
  await user.click(within(alert.closest("[role=alert]") as HTMLElement ?? document.body).getByRole("button", { name: p.instances.retry }));
  assert.ok(await groupOf("gpu-lab"));
  await waitFor(() => assert.equal(screen.queryAllByText(p.hpc.loadFailed).length, 0));
});

test("an account with no E-HPC cluster says so once the cloud has answered, after a reading mark", async () => {
  stub({ clusters: [] });
  mount();
  assert.ok(await screen.findByText(p.hpc.empty));
  assert.equal(screen.queryAllByText(p.detail.loadingHPCClusters).length, 0, "the reading mark is gone");
});

test("a key that may not list E-HPC clusters gets a quiet notice naming the RAM action, not an error", async () => {
  stub({ clusters: [], listing: { access: "permission_missing", permission: "ehpc:ListClusters" } });
  mount();
  assert.ok(await screen.findByText(p.hpc.noPermission.replace("{{permission}}", "ehpc:ListClusters")));
  assert.equal(screen.queryAllByText(p.hpc.loadFailed).length, 0, "it is no failure");
  assert.equal(screen.queryAllByText(p.hpc.empty).length, 0, "an empty list would say the account has none");
  assert.equal(screen.queryAllByRole("button", { name: p.instances.retry }).length, 0);
});

test("an account that never activated E-HPC says so with the cloud's word, and has no clusters", async () => {
  stub({ clusters: [], listing: { access: "not_activated", code: "ServiceNotActivated" } });
  mount();
  assert.ok(await screen.findByText(p.hpc.notActivated));
  assert.ok(screen.getByText(p.hpc.notActivatedCode.replace("{{code}}", "ServiceNotActivated")));
  assert.equal(screen.queryAllByText(p.hpc.loadFailed).length, 0);
});

test("regions that could not be read are a warning beside the clusters of the others", async () => {
  stub({ clusters: [gpuLab], listing: { partial: true, failed_regions: [
    { region: "cn-beijing", code: "Throttling.User", message: "flow control" },
    { region: "cn-hangzhou", code: "Forbidden.RAM", message: "not authorized", permission: "ehpc:ListClusters" },
  ] } });
  mount();
  assert.ok(await groupOf("gpu-lab"), "the clusters of the regions that answered are listed");
  assert.ok(screen.getByText(p.hpc.partial));
  const items = screen.getAllByRole("listitem").map((item) => item.textContent);
  assert.ok(items.includes("cn-beijing: Throttling.User"), String(items));
  assert.ok(items.includes(p.hpc.failedRegionPermission.replace("{{region}}", "cn-hangzhou").replace("{{permission}}", "ehpc:ListClusters")), String(items));
  assert.equal(screen.queryAllByText(p.hpc.loadFailed).length, 0);
});

test("the cluster list is read again after a sync, so a cluster created meanwhile appears", async () => {
  const calls = stub({ clusters: [] });
  const { sync } = mount();
  assert.ok(await screen.findByText(p.hpc.empty));
  assert.equal(calls.hpc.length, 1);
  Object.assign(api, { providerHPCClusters: async (...args: unknown[]) => { (calls.hpc ??= []).push(args); return hpcClusterList([gpuLab]); } });
  sync("2");
  assert.ok(await groupOf("gpu-lab"));
  assert.equal(calls.hpc.length, 2);
  assert.deepEqual(calls.hpc[1], [ali.id, true], "after a sync the cloud is read fresh");
  assert.equal(screen.queryAllByText(p.hpc.empty).length, 0);
});

test("in Chinese the group, the form, the plan and the outcome are in the operator's language", async () => {
  stub();
  const { user } = mount([], "zh");
  const d = await connect(user, "gpu-lab", zh.locale.slurm.hpc.connect);
  await user.click(d.getByRole("button", { name: zh.locale.slurm.form.submit }));
  await user.click(await d.findByRole("button", { name: zh.locale.slurm.created.applyNow }));
  const modal = within((await screen.findAllByRole("dialog")).at(-1) as HTMLElement);
  assert.ok(await modal.findByText(zh.locale.slurm.operation.checked));
  await user.click(modal.getByRole("button", { name: zh.locale.slurm.operation.confirm }));
  assert.ok(await modal.findByText(zh.locale.slurm.apply.done));
  assert.equal(document.body.textContent?.includes("{{"), false);
});

// A compute node's source link names the entry of its cluster: the group is brought into view when it is drawn, which
// is after the cloud's listing answers, and the listing may answer last.
test("a source link to an E-HPC entry lands on its group, whichever of the page's entries and the cloud's listing arrives last", async () => {
  const scrolled: string[] = [];
  const proto = (globalThis as unknown as { Element: { prototype: { scrollIntoView?: unknown } } }).Element.prototype;
  const before = proto.scrollIntoView;
  proto.scrollIntoView = function (this: HTMLElement) { scrolled.push(this.getAttribute("aria-label") ?? ""); };
  try {
    stub({ clusters: [hpcCluster("ehpc-sz-1", "gpu-lab", { entry: { uuid: "s-7", name: "gpu-lab" } })] });
    let release: () => void = () => undefined;
    const held = new Promise<void>((resolve) => { release = resolve; });
    const answer = api.providerHPCClusters;
    Object.assign(api, { providerHPCClusters: async (id: number) => { await held; return answer(id); } });
    mount([ehpcEntry({ name: "gpu-lab", slurm_cluster_name: "demo", node_count: 1 })], "en", { focusCluster: "s-7" });
    await screen.findByText(p.detail.loadingHPCClusters);
    assert.deepEqual(scrolled, [], "nothing to land on while the cloud has not answered");
    release();
    await groupOf("gpu-lab");
    await waitFor(() => assert.deepEqual(scrolled, ["gpu-lab"]));
  } finally {
    proto.scrollIntoView = before;
  }
});

// An E-HPC entry the platform can apply, in the group of its cluster, as the server describes it.
const ehpcSource = { provider_id: 3, provider_name: "ali-prod", provider_kind: "aliyun", provider_cluster_id: "ehpc-sz-1" };
const connectedLab = hpcCluster("ehpc-sz-1", "gpu-lab", { entry: { uuid: "s-7", name: "gpu-lab" } });
const waitingEntry = (over: Partial<SlurmCluster> = {}) => ehpcEntry({ name: "gpu-lab", source: ehpcSource, ...over });
const detailRow = async () => within((await groupRows(await groupOf("gpu-lab")))[0]);

test("the header's actions carry their names, and the next step while no node has joined is the filled button", async () => {
  stub({ clusters: [connectedLab] });
  mount([waitingEntry()]);
  const header = within(await groupHeader(await groupOf("gpu-lab")));
  const buttons = within(header.getByRole("group", { name: en.locale.common.resourceTable.actions })).getAllByRole("button");
  assert.deepEqual(buttons.map((button) => button.textContent), [s.actions.edit, s.actions.apply, en.locale.common.actions.more], "each says what it does, in words");
  const apply = header.getByRole("button", { name: s.actions.apply });
  assert.match(apply.className, /primary/, "applying leads while the cluster has no node");
  assert.doesNotMatch(header.getByRole("button", { name: s.actions.edit }).className, /primary/);
  assert.equal(header.getByRole("group", { name: en.locale.common.resourceTable.actions }).contains(apply), true);
  // Horizontal buttons: the icon and the name are the two parts of one content row, not an icon stacked over the name.
  for (const button of buttons) {
    const kids = [...(button.querySelector("[data-slot=button-content]") as HTMLElement).children].map((each) => each.getAttribute("data-slot"));
    assert.deepEqual(kids, ["button-leading-icon", "button-label"], `${button.textContent}: the icon sits left of the name`);
  }
});

test("once a node has joined the header still names its actions but nothing is filled", async () => {
  stub({ clusters: [connectedLab] });
  mount([waitingEntry({ slurm_cluster_name: "demo", node_count: 2, slurm_gpu_nodes: 2, missing_count: 0, missing_nodes: [] })]);
  const header = within(await groupHeader(await groupOf("gpu-lab")));
  assert.doesNotMatch(header.getByRole("button", { name: s.actions.apply }).className, /primary/);
  assert.equal(header.getByRole("button", { name: s.actions.apply }).textContent, s.actions.apply);
});

test("an entry the platform can apply leads with Check and apply and one line about where it runs, and shows no command until asked for by hand", async () => {
  const calls = stub({ clusters: [connectedLab] });
  const { user } = mount([waitingEntry()]);
  const row = await detailRow();
  assert.ok(row.getByText(s.detail.applyHelp), "one line says the platform runs it on the manager node");
  assert.ok(row.getByText(s.detail.waitingApply));
  assert.equal(row.queryAllByLabelText(s.setup.command, { selector: "pre" }).length, 0, "no command up front");
  assert.equal(row.queryAllByLabelText(s.setup.checkCommand, { selector: "pre" }).length, 0);
  assert.equal(document.body.textContent?.includes("curl -fsSL"), false);
  assert.equal(row.queryAllByText(s.detail.waiting).length, 0, "not the line that tells to run the command below");
  // The lead's own button is the same check-then-confirm modal as the header's.
  await user.click(row.getByRole("button", { name: s.actions.apply }));
  const modal = await dialog();
  assert.ok(await modal.findByText(s.operation.checked));
  assert.deepEqual(calls.check, [["s-7"]]);
  assert.equal(calls.apply, undefined, "nothing is applied before the operator confirms");
  await user.click(modal.getByRole("button", { name: s.operation.cancel }));
  await noDialog();
  // The commands are still there, folded under their own name.
  await user.click(row.getByRole("button", { name: s.detail.manual }));
  assert.equal((await row.findByLabelText(s.setup.command, { selector: "pre" })).textContent, SETUP_COMMAND);
  assert.equal(row.getByLabelText(s.setup.checkCommand, { selector: "pre" }).textContent, CHECK_COMMAND);
});

test("in Chinese the lead says 检查并应用 and 手动执行", async () => {
  stub({ clusters: [connectedLab] });
  mount([waitingEntry()], "zh");
  const row = await detailRow();
  const label = zh.locale.slurm.actions.apply;
  assert.equal(label, "检查并应用");
  assert.ok(row.getByRole("button", { name: label }));
  assert.ok(row.getByRole("button", { name: zh.locale.slurm.detail.manual }));
  assert.ok(row.getByText(zh.locale.slurm.detail.applyHelp));
});

test("an entry the platform cannot apply keeps its commands, folded under one line until opened; a revoked one shows the revoked line", async () => {
  stub({ clusters: [connectedLab] });
  mount([waitingEntry({ can_apply: false, can_remove: false, operation_blocked: "provider_unsupported" })]);
  const row = await detailRow();
  assert.ok(row.getByText(s.detail.waiting));
  assert.equal(row.queryAllByLabelText(s.setup.command, { selector: "pre" }).length, 0, "the commands start folded");
  const fold = row.getByRole("button", { name: s.detail.showCommands });
  assert.equal(fold.getAttribute("aria-expanded"), "false");
  fireEvent.click(fold);
  assert.equal((await row.findByLabelText(s.setup.command, { selector: "pre" })).textContent, SETUP_COMMAND);
  assert.equal(row.queryAllByText(s.detail.applyHelp).length, 0);
  cleanup();
  stub({ clusters: [connectedLab] });
  mount([waitingEntry({ revoked: true, state: "revoked", setup_command: undefined, check_command: undefined, can_apply: false, operation_blocked: "revoked" })]);
  const revoked = await detailRow();
  assert.ok(revoked.getByText(s.detail.revoked));
  assert.equal(revoked.queryAllByRole("button", { name: s.detail.showCommands }).length, 0);
});

const reasons = [
  { reason: "no_compute_nodes" },
  { reason: "not_applied" },
  { reason: "no_node_called" },
  { reason: "server_unreachable", hosts: [{ role: "node", name: "gpu000", status: "failed", reason: "timeout" }, { role: "node", name: "gpu001", status: "failed", reason: "dns" }] },
] as const;
const reasonText = (copy: typeof s, waiting: (typeof reasons)[number]) => {
  const hosts = "hosts" in waiting ? waiting.hosts.map((h) => `${h.name} (${copy.reach.reason[h.reason as "timeout" | "dns"]})`).join(", ") : "";
  return copy.waiting[waiting.reason].replace("{{url}}", "https://gpuworks.example").replace("{{hosts}}", hosts);
};

for (const language of ["en", "zh"] as const) {
  test(`in ${language} an entry that waits for its first node says why, as the server's code, in its detail and in the stalled badge's troubleshooting`, async () => {
    const copy = language === "en" ? s : zh.locale.slurm;
    for (const waiting of reasons) {
      stub({ clusters: [connectedLab] });
      const { user } = mount([waitingEntry({ stalled: true, state: "stalled", waiting: waiting as unknown as SlurmCluster["waiting"] })], language);
      const row = await detailRow();
      const text = reasonText(copy, waiting);
      assert.ok(text.length > 20 && !text.includes("{{"), text);
      assert.ok(row.getByText(text), `${waiting.reason}: the detail says why`);
      assert.equal(row.queryAllByText(copy.detail.waitingApply).length, 0, "the reason is the one line, not the generic one beside it");
      if (waiting.reason === "server_unreachable") for (const host of waiting.hosts) assert.ok(text.includes(host.name));
      await user.click(within(await groupHeader(await groupOf("gpu-lab"))).getByRole("button", { name: copy.status.troubleshoot }));
      const popover = within(await screen.findByText(copy.stalled.title).then((title) => title.closest("[role=dialog]") as HTMLElement));
      assert.ok(popover.getByText(text), `${waiting.reason}: the troubleshooting says why`);
      assert.equal(popover.queryAllByText(copy.stalled.intro).length, 0, "the reason replaces the general intro");
      assert.ok(popover.getByText(copy.stalled.causes), "the commands and the usual causes stay");
      cleanup();
    }
  });
}

test("an entry whose waiting the server cannot explain says the general line", async () => {
  stub({ clusters: [connectedLab] });
  mount([waitingEntry({ waiting: undefined })]);
  const row = await detailRow();
  assert.ok(row.getByText(s.detail.waitingApply));
});

const unreachable: SlurmReach = {
  hosts: [
    { role: "controller", name: "ctl", status: "ok" },
    { role: "node", name: "gpu000", status: "failed", reason: "timeout" },
    { role: "node", name: "gpu001", status: "ok" },
    { role: "node", name: "gpu002", status: "unknown", reason: "assistant_offline" },
  ],
  nodes_unlisted: "permission_missing", nodes_permission: "ehpc:ListNodes",
};

test("a check that found a host that cannot reach the server says which, why and where, before anything is applied", async () => {
  stub({ check: () => plan({ status: "partial", warnings: ["server_unreachable"], reach: unreachable }) });
  const { user } = mount();
  const { modal } = await createThenApply(user);
  assert.ok(await modal.findByText(s.operation.checkedPartial));
  const warning = within(modal.getByText(s.plan.attention).parentElement as HTMLElement).getAllByRole("listitem").map((item) => item.textContent);
  assert.deepEqual(warning, [s.plan.warning.server_unreachable.replace("{{url}}", "https://gpuworks.example")]);
  assert.ok(modal.getByText(s.reach.title));
  const failed = within(modal.getByRole("list", { name: s.reach.failedList })).getAllByRole("listitem").map((item) => item.textContent);
  assert.deepEqual(failed, ["Compute node gpu000 cannot reach https://gpuworks.example: timed out"]);
  assert.ok(modal.getByText("gpu002 (Cloud Assistant is offline)"), "a node that could not be asked is said, not counted as failed");
  assert.ok(modal.getByText(s.reach.unlisted.permission_missing.replace("{{permission}}", "ehpc:ListNodes")));
  assert.equal((modal.getByRole("button", { name: s.operation.confirm }) as HTMLButtonElement).disabled, false, "the operator decides");
});

test("a check where every host reaches the server says so in one line", async () => {
  stub({ check: () => plan({ reach: { hosts: [{ role: "controller", name: "ctl", status: "ok" }, { role: "node", name: "gpu000", status: "ok" }] } }) });
  const { user } = mount();
  const { modal } = await createThenApply(user);
  assert.ok(await modal.findByText("2 hosts reach https://gpuworks.example."));
  assert.equal(modal.queryAllByRole("list", { name: s.reach.failedList }).length, 0);
});

test("a plan without any host asked says nothing of the server's address", async () => {
  stub();
  const { user } = mount();
  const { modal } = await createThenApply(user);
  assert.ok(await modal.findByText(s.operation.checked));
  assert.equal(modal.queryAllByText(s.reach.title).length, 0);
});

test("an apply that left hosts unable to reach the server lists them in the outcome and in what is left to do", async () => {
  stub({ apply: () => outcome({
    status: "partial", reach: unreachable,
    steps: [{ id: "gres", status: "pass" }, { id: "check_script", status: "pass" }, { id: "distribute", status: "skip" }, { id: "slurm_conf", status: "pass" }, { id: "reconfigure", status: "pass" }, { id: "resume", status: "pass" }, { id: "reach", status: "warn" }],
  }) });
  const { user } = mount();
  const { modal } = await createThenApply(user);
  await user.click(await modal.findByRole("button", { name: s.operation.confirm }));
  assert.ok(await modal.findByText(s.apply.partial));
  const todo = within(modal.getByText(s.apply.todoTitle).nextElementSibling as HTMLElement).getAllByRole("listitem").map((item) => item.textContent);
  assert.deepEqual(todo, [s.apply.todo.reach.replace("{{nodes}}", "gpu000").replace("{{url}}", "https://gpuworks.example")]);
  assert.equal(within(modal.getByRole("list", { name: s.reach.failedList })).getAllByRole("listitem")[0].textContent, "Compute node gpu000 cannot reach https://gpuworks.example: timed out");
  assert.ok(stepsShown(modal).includes("Needs attentionReach the server's address from the hosts"), String(stepsShown(modal)));
});

test("in Chinese a host that cannot reach the server is said as 计算节点 gpu000 访问不到 …：连接超时", async () => {
  stub({ check: () => plan({ status: "partial", warnings: ["server_unreachable"], reach: unreachable }) });
  const { user } = mount([], "zh");
  const d = await connect(user, "gpu-lab", zh.locale.slurm.hpc.connect);
  await user.click(d.getByRole("button", { name: zh.locale.slurm.form.submit }));
  await user.click(await d.findByRole("button", { name: zh.locale.slurm.created.applyNow }));
  const modal = within((await screen.findAllByRole("dialog")).at(-1) as HTMLElement);
  assert.ok(await modal.findByText("计算节点 gpu000 访问不到 https://gpuworks.example：连接超时"));
  assert.ok(modal.getByText("gpu002 (云助手不在线)"));
});

test("the form warns, in one line, when the typed address is the platform's own temporary public address, and does not refuse it", async () => {
  const calls = stub({ defaults: { quick_tunnel_url: "http://localhost" } });
  const { user } = mount();
  const d = await connect(user);
  assert.ok(await d.findByText(s.form.serverUrlQuick));
  assert.equal(d.queryAllByText(s.form.serverUrlHelp).length, 0, "the warning is the field's one line");
  // Another address: the usual line comes back.
  const field = d.getByLabelText(/^Management node address/) as HTMLInputElement;
  await user.clear(field);
  await user.type(field, "https://gw.example.com:8800");
  await waitFor(() => assert.equal(d.queryAllByText(s.form.serverUrlQuick).length, 0));
  assert.ok(d.getByText(s.form.serverUrlHelp));
  // Typing the temporary address again, with a trailing slash: still warned, and still allowed.
  await user.clear(field);
  await user.type(field, "http://localhost/");
  assert.ok(await d.findByText(s.form.serverUrlQuick));
  await user.click(d.getByRole("button", { name: s.form.submit }));
  assert.ok(await d.findByText(s.created.title.replace("{{name}}", "gpu-lab")));
  assert.equal((calls.create[0][0] as SlurmClusterRequest).server_url, "http://localhost/");
});

test("with no temporary public address the form says only its usual line", async () => {
  stub();
  const { user } = mount();
  const d = await connect(user);
  assert.ok(await d.findByText(s.form.serverUrlHelp));
  assert.equal(d.queryAllByText(s.form.serverUrlQuick).length, 0);
});

test("a stalled entry the platform applies is troubleshot by Check and apply, not on the controller: no controller command, the node's log check stays", async () => {
  stub({ clusters: [connectedLab] });
  const { user } = mount([waitingEntry({ stalled: true, state: "stalled", install_dir: "/opt/gw" })]);
  await user.click(within(await groupHeader(await groupOf("gpu-lab"))).getByRole("button", { name: s.status.troubleshoot }));
  const popover = within(await screen.findByText(s.stalled.title).then((title) => title.closest("[role=dialog]") as HTMLElement));
  assert.ok(popover.getByText(s.stalled.introApplied), "the first step is Check and apply");
  assert.ok(popover.getByText(s.stalled.notRunApplied));
  assert.equal(popover.queryAllByText(s.stalled.notRun).length, 0, "no \"run the setup command on the controller\"");
  assert.equal(popover.queryAllByText(s.stalled.intro).length, 0);
  assert.equal(popover.queryAllByLabelText(s.stalled.on.controller, { selector: "pre" }).length, 0, "no controller command");
  assert.ok((popover.getByLabelText(s.stalled.on.node, { selector: "pre" }).textContent ?? "").includes("tail -n 50 /opt/gw/slurm-check.log"));
  assert.doesNotMatch(popover.getByRole("heading", { hidden: true, name: s.stalled.title }).parentElement?.textContent ?? "", /scontrol show config/);
});

test("an entry the platform cannot apply is still troubleshot on the controller, where its command is the way in", async () => {
  stub({ clusters: [connectedLab] });
  const { user } = mount([waitingEntry({ stalled: true, state: "stalled", can_apply: false, operation_blocked: "provider_unsupported" })]);
  await user.click(within(await groupHeader(await groupOf("gpu-lab"))).getByRole("button", { name: s.status.troubleshoot }));
  const popover = within(await screen.findByText(s.stalled.title).then((title) => title.closest("[role=dialog]") as HTMLElement));
  assert.ok(popover.getByText(s.stalled.intro));
  assert.ok(popover.getByText(s.stalled.notRun));
  assert.equal(popover.getByLabelText(s.stalled.on.controller, { selector: "pre" }).textContent, "scontrol show config | grep -i HealthCheck");
});

test("the check and apply dialog is titled with the cluster's name in words, with no separator glyph a font may lack", async () => {
  stub();
  const { user } = mount();
  const { modal } = await createThenApply(user);
  const title = modal.getByRole("heading", { name: s.operation.title.replace("{{name}}", "gpu-lab") });
  assert.doesNotMatch(title.textContent ?? "", /[\u00b7\u2022\u25a1]/, title.textContent ?? "");
  assert.match(title.textContent ?? "", /gpu-lab/);
  assert.doesNotMatch(zh.locale.slurm.operation.title, /[\u00b7\u2022]/);
  assert.doesNotMatch(zh.locale.slurm.uninstall.title, /[\u00b7\u2022]/);
});

test("in Chinese the cluster's own node is the Slurm 管控节点 and 管理节点 is only gpuworks' address: the failed-apply error reads that way, and no cluster text mixes them", async () => {
  const { apiErrorText, APIError } = await import("../src/apiError.ts");
  const t = zh.i18n.getFixedT("zh", "common");
  const tEn = en.i18n.getFixedT("en", "common");
  const unreachable = new APIError("SLURM_APPLY_FAILED", { step: "setup", reason: "server_unreachable", output: "", url: "http://203.0.113.9:8800", kind: "timeout" }, "x", 424);
  assert.equal(
    apiErrorText(t as never, unreachable),
    "配置没有完成（配置健康检查）：Slurm 管控节点访问不到管理节点接入地址 http://203.0.113.9:8800（连接超时）。节点要能访问这个地址：集群所在 VPC 没有公网出口时配置 NAT 网关，或把 gpuworks 部署在同一 VPC 内并填写内网地址。",
  );
  assert.equal(
    apiErrorText(tEn as never, unreachable),
    "The setup on the Slurm manager node did not finish (setting up the health check): the Slurm manager node cannot reach the management node address http://203.0.113.9:8800 (connection timed out). The cluster's nodes must reach it too: add a NAT gateway when the cluster's VPC has no public egress, or host gpuworks in the same VPC and enter its private address.",
  );
  // An older server names no address: the wording still says which address and what to do.
  assert.match(
    apiErrorText(t as never, new APIError("SLURM_APPLY_FAILED", { step: "setup", reason: "server_unreachable", output: "" }, "x", 424)),
    /^配置没有完成（配置健康检查）：Slurm 管控节点访问不到管理节点接入地址。.*NAT 网关/,
  );
  const collect = (value: unknown, texts: string[]): string[] => { if (typeof value === "string") texts.push(value); else if (value && typeof value === "object") Object.values(value).forEach((v) => collect(v, texts)); return texts; };
  const common = zh.locale.common as Record<string, Record<string, unknown>>;
  const texts = collect(zh.locale.slurm, []);
  collect(common.slurmApplyStep, texts); collect(common.slurmApplyReason, texts); collect(common.slurmApplyKind, texts); collect(common.slurmApplyNotApplicable, texts);
  for (const [code, text] of Object.entries(common.apiError as Record<string, string>)) if (code.startsWith("SLURM_")) texts.push(text);
  assert.ok(texts.length > 100);
  // "GPUWorks 的管理节点" is the server itself, as the other connect forms say.
  for (const text of texts) assert.doesNotMatch(text.replaceAll("GPUWorks 的管理节点", ""), /管理节点(?!接入地址)/, `管理节点 is the gpuworks server only: ${text}`);
  // The cluster's own node has one name, so it cannot be taken for the 管理节点 beside it: never the bare
  // 管控节点, never the 控制节点 the on-prem texts used to say, and the name is not doubled.
  const slurmTexts = [...texts, ...collect(zh.locale.providers, []).filter((text) => text.includes("Slurm"))];
  for (const text of slurmTexts) {
    assert.doesNotMatch(text, /(?<!Slurm )管控节点/, `a bare 管控节点: ${text}`);
    assert.doesNotMatch(text, /控制节点/, `控制节点 is called Slurm 管控节点 here: ${text}`);
    assert.doesNotMatch(text, /Slurm Slurm|Slurm 管控节点Slurm/, `the name is doubled: ${text}`);
  }
  // English says the same one thing, in the places that name that node.
  const enCommon = en.locale.common as Record<string, Record<string, unknown>>;
  const enTexts = [...collect(en.locale.slurm, []), ...collect(enCommon.slurmApplyStep, []), ...collect(enCommon.slurmApplyReason, []), ...collect(enCommon.slurmApplyNotApplicable, [])];
  for (const [code, text] of Object.entries(enCommon.apiError as Record<string, string>)) if (code.startsWith("SLURM_")) enTexts.push(text);
  for (const text of enTexts) assert.doesNotMatch(text, /(?<!Slurm )manager node|Slurm controller|Slurm Slurm/, `the node is the Slurm manager node: ${text}`);
});

test("a joined E-HPC entry the server suggests it for offers to register the cluster's NAS as model storage; one without the suggestion does not", async () => {
  stub({ clusters: [connectedLab] });
  const hint = { name: "gpu-lab-models", type: "shared_dir", path: "/opt/gpuworks-models", label_key: "gpuworks.ai/slurm-cluster", label_value: "s-7" };
  mount([waitingEntry({ node_count: 1, online_count: 1, slurm_gpu_nodes: 1, missing_count: 0, missing_nodes: [], shared_storage_hint: hint })]);
  const row = await detailRow();
  assert.ok(row.getByText(s.sharedStorage.line.replace("{{path}}", "/opt/gpuworks-models")));
  assert.ok(row.getByRole("button", { name: s.sharedStorage.register }));
  cleanup();
  stub({ clusters: [connectedLab] });
  mount([waitingEntry({ node_count: 1, online_count: 1, slurm_gpu_nodes: 1, missing_count: 0, missing_nodes: [] })]);
  const plain = await detailRow();
  assert.equal(plain.queryAllByRole("button", { name: s.sharedStorage.register }).length, 0);
});

test("the last listing shows at once while the server reads the cloud again, and the new one replaces it when it is in", async () => {
  const calls = stub({ clusters: [] });
  let answers = 0;
  Object.assign(api, { providerHPCClusters: async (...args: unknown[]) => {
    (calls.hpc ??= []).push(args);
    answers += 1;
    return answers === 1 ? hpcClusterList([], { refreshing: true }) : hpcClusterList([gpuLab]);
  } });
  mount();
  assert.ok(await screen.findByText(p.hpc.empty), "the old listing is shown at once");
  assert.ok(await groupOf("gpu-lab"), "the refreshed listing replaces it");
  assert.equal(calls.hpc.length, 2);
  assert.deepEqual(calls.hpc[1], [ali.id, undefined], "asking again is not a fresh read");
});

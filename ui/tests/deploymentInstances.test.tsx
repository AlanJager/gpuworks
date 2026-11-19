// The shared fixture installs an isolated DOM and the original cleanup hooks in each test process.
import { assert, test, screen, waitFor, within, fireEvent, userEvent, list, listPage, dom, i18n, locale, Events, scrolled, APIError, selectInstance, stub, instance, deployment, openDetail, drawer } from "./helpers/deploymentLifecycle.tsx";

test("an ordinary replica that gave up is named with its failure, and Retry targets that replica", async () => {
  const exhausted = deployment({ instances: [instance("inst-a", { replica: { index: 0 } })], retry_exhausted: { index: 1, failed_attempts: 3, last_failure: "engine exited" } });
  stub("deploymentsPage", async () => listPage([exhausted]));
  stub("deployment", async () => exhausted);
  const calls: Array<{ id: number; unit: unknown }> = [];
  let fail = true;
  stub("retryDeploymentUnit", async (id: number, unit: unknown) => {
    calls.push({ id, unit });
    if (fail) throw new APIError("UNIT_NOT_FAILED", { role: "", index: 1 }, "the unit has no failed attempts to retry", 409);
    return deployment();
  });
  await openDetail();
  const overview = screen.getByRole("heading", { name: locale.detail.serviceOverview }).closest("section")!;
  const status = within(overview).getByText(locale.detail.state).closest("tr")! as HTMLElement;
  const alert = within(status).getByRole("alert");
  assert.ok(within(alert).getByText("A replica stopped being rebuilt after 3 failed attempts: engine exited"));
  fireEvent.click(within(alert).getByRole("button", { name: locale.detail.retry.action }));
  await within(alert).findByText(i18n.t("detail.retry.failed", { detail: i18n.t("common:apiError.UNIT_NOT_FAILED") }));
  fail = false;
  // antd leaves the loading icon's leave animation in the button's name under
  // JSDOM, so find the second click's button by its text.
  fireEvent.click(within(alert).getByText(locale.detail.retry.action).closest("button")!);
  await waitFor(() => assert.equal(calls.length, 2));
  assert.deepEqual(calls[1], { id: 1, unit: { role: "", index: 1 } });
});

test("a deployment without a give-up shows no retry notice", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  await openDetail();
  assert.equal(screen.queryByText(/stopped being rebuilt/), null);
});

test("instance switching starts with the first-created instance and retains the leader and rank of each follower", async () => {
  const group = (uuid: string, rank: number, group: string, index: number) => instance(uuid, {
    nnodes: 2, node_rank: rank, headless: rank > 0, dist_role: rank > 0 ? "follower" : "leader", dist_group: group, dist_attempt: 1, replica: { index },
  });
  // Listed follower-first and interleaved; a legacy follower row carries no
  // group, and the server still names it a follower (it derives dist_role
  // from headless and the rank for rows from before it was recorded).
  const multi = deployment({ instances: [
    group("follow-a1", 1, "7-a1f3", 0), group("follow-b1", 1, "7-c9e0", 1), group("leader-a0", 0, "7-a1f3", 0), group("leader-b0", 0, "7-c9e0", 1),
    instance("legacy-f1", { nnodes: 2, node_rank: 1, headless: true, dist_role: "follower" }),
  ] });
  stub("deploymentsPage", async () => listPage([multi]));
  stub("deployment", async () => multi);
  await openDetail();
  selectInstance("legacy-f1");
  for (const [uuid, rank] of [
    ["legacy-f1", "follower · rank 1"],
    ["leader-b0", "leader · rank 0"],
    ["leader-a0", "leader · rank 0"],
    ["follow-b1", "follower · rank 1 · leader leader-b"],
    ["follow-a1", "follower · rank 1 · leader leader-a"],
  ]) {
    selectInstance(uuid);
    assert.deepEqual([...document.querySelectorAll("[id^='instance-']")].map((el) => el.id), [`instance-${uuid}`]);
    assert.ok(screen.getByText(rank));
  }
});

test("status filters count the whole deployment and follow matching records across replicas and refreshes", async () => {
  const generation = { nnodes: 2, dist_group: "multi-node", dist_attempt: 1 };
  let rows = [instance("ready", { ...generation, replica: { index: 0 } }), instance("launch", { replica: { index: 1 }, state: "starting" }),
    instance("retry", { ...generation, dist_role: "follower", node_rank: 1, state: "failed" }), instance("unassigned", { state: "failed" })];
  stub("deployment", async () => deployment({ instances: rows }));
  await openDetail();
  selectInstance("ready");
  const all = () => screen.getByRole("button", { name: "All 4" });
  const replica = () => screen.getByRole("combobox", { name: locale.detailLayout.switchReplica });
  fireEvent.click(screen.getByRole("button", { name: "Starting 1" }));
  assert.ok(document.getElementById("instance-launch"));
  assert.ok(all());
  fireEvent.click(replica());
  assert.equal(screen.getAllByRole("option").length, 1);
  fireEvent.click(screen.getByRole("option"));

  fireEvent.click(screen.getByRole("button", { name: "Failed attempt 2" }));
  assert.ok(document.getElementById("instance-retry"));
  fireEvent.click(replica());
  const options = screen.getAllByRole("option");
  assert.equal(options.length, 2);
  fireEvent.click(options[1]);
  assert.ok(document.getElementById("instance-unassigned"));
  assert.ok(screen.getByRole("button", { name: "Failed attempt 2", pressed: true }));
  fireEvent.click(all());
  assert.ok(document.getElementById("instance-unassigned"));

  fireEvent.click(screen.getByRole("button", { name: "Starting 1" }));
  rows = rows.filter((_, i) => i !== 1);
  for (const listener of Events.listeners.get("instance") ?? []) listener(new dom.window.Event("instance"));
  await screen.findByRole("button", { name: "All 3", pressed: true });
  assert.ok(document.getElementById("instance-ready"));
});

test("the cascade keeps the inspected attempt across refreshes and searches all 32 records by status and ID", async () => {
  const record = (uuid: string, state: string) => instance(uuid, {
    state, replica: state === "running" ? { index: 0 } : undefined, schedule: { unit: { key: "|[]" } },
  });
  let rows = [record("ready", "running"), record("first-failure", "failed")];
  stub("deployment", async () => deployment({ instances: rows }));
  stub("deploymentsPage", async () => listPage([deployment({ instances: rows })]));
  const refresh = () => {
    for (const listener of Events.listeners.get("instance") ?? []) listener(new dom.window.Event("instance"));
  };
  await openDetail();
  selectInstance("first-failure");
  rows = [...Array.from({ length: 30 }, (_, n) => record(`attempt-${n}`, "failed")), ...rows];
  refresh();
  const failures = await screen.findByRole("button", { name: "Failed attempt 31" });
  assert.ok(document.getElementById("instance-first-failure"));
  fireEvent.click(failures);
  const switcher = within(screen.getByRole("group", { name: locale.detail.switchInstance }));
  fireEvent.click(switcher.getByRole("combobox"));
  fireEvent.change(screen.getByPlaceholderText(locale.detail.switchInstance), { target: { value: "ATTEMPT-29" } });
  const options = within(screen.getByRole("listbox")).getAllByRole("option");
  assert.deepEqual(options.map(el => el.getAttribute("aria-label")), ["attempt-29 Failed attempt"]);
  fireEvent.click(options[0]);
  assert.ok(document.getElementById("instance-attempt-29"));
  fireEvent.click(switcher.getByRole("button", { name: locale.detail.previousInstance }));
  assert.ok(document.getElementById("instance-attempt-28"));
  fireEvent.click(switcher.getByRole("button", { name: locale.detail.nextInstance }));
  assert.ok(document.getElementById("instance-attempt-29"));
  rows = [];
  refresh();
  await within(document.getElementById("deployment-instances")!).findByText(locale.detail.noInstances);
  assert.ok(screen.getByRole("tab", { name: locale.detail.instanceDetails, exact: true }));
  assert.equal(drawer().querySelector(".replica-cascade"), null);
  assert.equal(screen.queryByRole("group", { name: locale.detail.switchInstance }), null);
});

test("the overview copies the model, public name and configured image independently of the model link and instance image", async () => {
  const image = "registry.example:5000/engines/runner:v2";
  const dep = deployment({ model_name: "org/model", served_name: "public-model", image });
  stub("deploymentsPage", async () => listPage([dep]));
  stub("deployment", async () => dep);
  await openDetail();
  const model = within(within(drawer()).getByRole("heading", { name: locale.detail.serviceOverview }).closest("section")!);
  const user = userEvent.setup();
  for (const [label, value] of [
    [locale.detail.copyModel, dep.model_name],
    [locale.detail.copyServedName, dep.served_name],
    [locale.detail.copyConfiguredImage, image],
  ]) {
    await user.click(model.getByRole("button", { name: label }));
    assert.equal(await navigator.clipboard.readText(), value);
  }
  assert.equal(model.getByRole("link", { name: dep.model_name }).getAttribute("href"), `/models?detail=${dep.model_id}`);
});

test("an unplaced deployment shows its engine and automatic image selection before any instance exists", async () => {
  const dep = deployment({ runtime_state: "pending", instances: [] });
  stub("deploymentsPage", async () => listPage([dep]));
  stub("deployment", async () => dep);
  await openDetail();
  const model = within(drawer()).getByRole("group", { name: locale.detail.modelAndEngine });
  const field = within(model).getByText(locale.detail.configuredRuntime).closest("tr")! as HTMLElement;
  assert.ok(within(field).getByText(dep.engine));
  assert.ok(within(field).getByText(locale.page.autoImage));
  assert.equal(within(field).queryByRole("button", { name: locale.detail.copyConfiguredImage }), null);
  assert.equal(within(drawer()).queryByRole("heading", { name: locale.detail.instanceLaunch }), null);
});

test("instance metrics follow selection and the remaining record after removal", async () => {
  const { installCanvas } = await import("./helpers/analyticsFixtures.ts");
  installCanvas(dom);
  let rows = [instance("third", { state: "failed" }), instance("second"), instance("first")];
  const queried: string[] = [];
  stub("metricsQuery", async (_metric: string, params: { instance?: string }) => {
    if (params.instance) queried.push(params.instance);
    return list([]);
  });
  stub("deploymentsPage", async () => listPage([deployment({ instances: rows })]));
  stub("deployment", async () => deployment({ instances: rows }));
  await openDetail();
  selectInstance("first");
  assert.ok(screen.getByRole("heading", { name: locale.instance.tab.metrics }));
  selectInstance("second");
  await waitFor(() => assert.ok(queried.includes("second")));
  rows = [rows[0], rows[2]];
  for (const listener of Events.listeners.get("instance") ?? []) listener(new dom.window.Event("instance"));
  await waitFor(() => assert.ok(document.getElementById("instance-first")));
  assert.equal(document.getElementById("instance-second"), null);
  assert.ok(screen.getByRole("heading", { name: locale.instance.tab.metrics }));
});

test("single-rank replicas carry no rank tag and keep the plain count", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  await openDetail();
  await screen.findByText(locale.detail.instances.replace("{{count}}", "2"));
  assert.equal(screen.queryByText(/rank \d/), null);
});

test("the monitor's instance link scrolls the drawer to that instance", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  await openDetail("?deployment=1&instance=inst-a");
  await waitFor(() => assert.deepEqual(scrolled, ["deployment-instances"]));
  assert.ok(document.getElementById("instance-inst-a"));
  selectInstance("inst-b");
  assert.ok(document.getElementById("instance-inst-b"));
  selectInstance("inst-a");
  assert.ok(document.getElementById("instance-inst-a"));
  assert.deepEqual(scrolled, ["deployment-instances"]);
});

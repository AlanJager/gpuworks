// The shared fixture installs an isolated DOM and the original cleanup hooks in each test process.
import { assert, test, screen, waitFor, within, cleanup, fireEvent, userEvent, listPage, i18n, locale, selectInstance, stub, instance, deployment, openDetail, multiNodeDeployment } from "./helpers/deploymentLifecycle.tsx";

test("a replica's instance offers Restart (draining by default) and Remove; restart posts drain unless unticked", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  const restarts: Array<{ uuid: string; drain: boolean }> = [];
  stub("restartInstance", async (uuid: string, drain: boolean) => { restarts.push({ uuid, drain }); return deployment(); });
  await openDetail();
  selectInstance("inst-b");
  const panel = screen.getByRole("group", { name: locale.detailLayout.replicaControls });
  fireEvent.click(within(panel).getByRole("button", { name: locale.replica.restart }));
  let modal = (await screen.findByText(i18n.t("replica.restartNamed", {label: i18n.t("replica.unitLabel", {index: 2})}))).closest(".ant-modal") as HTMLElement;
  const drain = within(modal).getByRole("checkbox", { name: locale.replica.drain }) as HTMLInputElement;
  assert.equal(drain.getAttribute("aria-checked"), "true", "draining is the default");
  fireEvent.click(within(modal).getByRole("button", { name: locale.replica.restart }));
  await waitFor(() => assert.deepEqual(restarts, [{ uuid: "inst-b", drain: true }]));

  fireEvent.click(within(panel).getByRole("button", { name: locale.replica.restart }));
  modal = (await screen.findAllByText(i18n.t("replica.restartNamed", {label: i18n.t("replica.unitLabel", {index: 2})}))).map((el) => el.closest(".ant-modal") as HTMLElement).find((el) => !el.closest(".ant-zoom-leave")) as HTMLElement;
  await userEvent.click(within(modal).getByRole("checkbox", { name: locale.replica.drain }));
  // The reopened dialog's OK button keeps antd's loading-icon leave
  // animation in its name under JSDOM; find it by its text.
  fireEvent.click(within(modal).getByText(locale.replica.restart).closest("button")!);
  await waitFor(() => assert.deepEqual(restarts[1], { uuid: "inst-b", drain: false }));
});

test("removing a replica scales down by one naming that instance", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  const scales: unknown[] = [];
  stub("scaleReplicas", async (id: number, body: unknown) => { scales.push({ id, body }); return deployment({ replicas: 1 }); });
  await openDetail();
  selectInstance("inst-a");
  const panel = screen.getByRole("group", { name: locale.detailLayout.replicaControls });
  fireEvent.click(within(panel).getByRole("button", { name: locale.replica.remove }));
  const confirm = (await screen.findByText(/^Remove replica .*\?/i)).closest(".ant-popover") as HTMLElement;
  fireEvent.click(within(confirm).getByRole("button", { name: locale.replica.remove }));
  await waitFor(() => assert.deepEqual(scales, [{ id: 1, body: { replicas: 1, remove: ["inst-a"] } }]));
});

test("a single-replica deployment, a failed attempt and a follower carry no replica buttons", async () => {
  const single = deployment({ replicas: 1, instances: [instance("only", { replica: { index: 0 } }), instance("failed-try", { state: "failed" })] });
  stub("deploymentsPage", async () => listPage([single]));
  stub("deployment", async () => single);
  await openDetail();
  assert.equal(screen.queryByRole("button", { name: locale.replica.restart }), null);
  assert.equal(screen.queryByRole("button", { name: locale.replica.remove }), null);
  cleanup();

  const multi = deployment({ replicas: 2, nnodes: 2, instances: [
    instance("lead", { nnodes: 2, node_rank: 0, dist_role: "leader", dist_group: "g", dist_attempt: 1, replica: { index: 0 } }),
    instance("follow", { nnodes: 2, node_rank: 1, headless: true, dist_role: "follower", dist_group: "g", dist_attempt: 1, replica: { index: 0 } }),
    instance("failed-lead", { state: "failed", nnodes: 2, node_rank: 0, dist_role: "leader", dist_group: "h", dist_attempt: 1 }),
  ] });
  stub("deploymentsPage", async () => listPage([multi]));
  stub("deployment", async () => multi);
  await openDetail();
  selectInstance("lead");
  const lead = screen.getByRole("group", { name: locale.detailLayout.replicaControls });
  assert.ok(within(lead).getByRole("button", { name: locale.replica.restart }));
  // Without the replica list the response carries, there is nothing to
  // remove the replica from.
  assert.equal(within(lead).queryByRole("button", { name: locale.replica.remove }), null);
  selectInstance("follow");
  const follower = document.getElementById("instance-follow") as HTMLElement;
  assert.equal(within(follower).queryByRole("button", { name: locale.replica.restart }), null);
  assert.ok(within(follower).getByText(locale.replica.followerHint));
  selectInstance("failed-lead");
  assert.equal(within(document.getElementById("instance-failed-lead") as HTMLElement).queryByRole("button", { name: locale.replica.restart }), null);
});

test("a single multi-node replica explains its follower even without replica actions", async () => {
  const single = multiNodeDeployment();
  stub("deploymentsPage", async () => listPage([single]));
  stub("deployment", async () => single);
  await openDetail();
  selectInstance("follow-b");
  const follower = within(screen.getByRole("region", { name: locale.detail.instanceDetails }));
  assert.ok(follower.getByText("A follower rank: it is restarted, removed, slept and woken with its replica's leader."));
  assert.ok(follower.getByText("follower · rank 1 · leader lead-a"));
  const controls = within(screen.getByRole("group", { name: locale.detailLayout.replicaControls }));
  assert.equal(controls.queryByRole("button", { name: locale.replica.restart }), null);
  assert.equal(controls.queryByRole("button", { name: locale.replica.remove }), null);
  selectInstance("lead-a");
  const leader = within(screen.getByRole("region", { name: locale.detail.instanceDetails }));
  assert.ok(leader.getByText("leader · rank 0"));
  assert.equal(leader.queryByText(locale.replica.followerHint), null);
});

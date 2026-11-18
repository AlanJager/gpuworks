// The shared fixture installs an isolated DOM and the original cleanup hooks in each test process.
import { assert, test, React, I18nextProvider, MemoryRouter, render, screen, waitFor, within, fireEvent, userEvent, listPage, i18n, locale, APIError, DeploymentsPage, stub, deployment, openDetail, drawer } from "./helpers/deploymentLifecycle.tsx";

test("an active deployment offers Restart and Stop, not Start; canceling the restart posts nothing", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  let restarted = 0;
  stub("restartDeployment", async () => { restarted += 1; return deployment(); });
  await openDetail();
  assert.equal(within(drawer()).queryByRole("button", { name: locale.detail.start }), null);
  fireEvent.click(within(drawer()).getByRole("button", { name: locale.detail.restart.action }));
  const modal = (await screen.findByText(locale.detail.restart.confirmTitle)).closest(".ant-modal") as HTMLElement;
  // Rolling is the default and says what it does.
  assert.ok((within(modal).getByRole("radio", { name: locale.detail.restart.strategyRolling }) as HTMLInputElement).checked);
  assert.ok(within(modal).getByText(locale.detail.restart.rollingDescription));
  fireEvent.click(within(modal).getByRole("button", { name: locale.detail.restart.cancel }));
  await Promise.resolve();
  assert.equal(restarted, 0);
});

test("confirming the restart posts once and refetches the drawer", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  let loads = 0;
  stub("deployment", async () => { loads += 1; return deployment(); });
  const calls: Array<[number, string]> = [];
  stub("restartDeployment", async (id: number, strategy: string) => { calls.push([id, strategy]); return deployment(); });
  await openDetail();
  const before = loads;
  fireEvent.click(within(drawer()).getByRole("button", { name: locale.detail.restart.action }));
  const modal = (await screen.findByText(locale.detail.restart.confirmTitle)).closest(".ant-modal") as HTMLElement;
  fireEvent.click(within(modal).getByRole("button", { name: locale.detail.restart.confirmOk }));
  await waitFor(() => assert.deepEqual(calls, [[1, "rolling"]]));
  await waitFor(() => assert.ok(loads > before, "the drawer refetches after the restart"));
});

test("a refused restart is shown inside the confirm dialog", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  stub("deployment", async () => deployment());
  stub("restartDeployment", async () => { throw new APIError("DEPLOYMENT_NOT_ACTIVE", { state: "stopped" }, "only an active deployment can be restarted", 409); });
  await openDetail();
  fireEvent.click(within(drawer()).getByRole("button", { name: locale.detail.restart.action }));
  const modal = (await screen.findByText(locale.detail.restart.confirmTitle)).closest(".ant-modal") as HTMLElement;
  fireEvent.click(within(modal).getByRole("button", { name: locale.detail.restart.confirmOk }));
  const refusal = i18n.t("detail.restart.failed", { detail: i18n.t("common:apiError.DEPLOYMENT_NOT_ACTIVE", { state: "stopped" }) });
  await within(modal).findByText(refusal);
  // The refusal answered that attempt: cancelled and opened again, the
  // dialog asks afresh.
  fireEvent.click(within(modal).getByText(locale.detail.cancel).closest("button") as HTMLElement);
  fireEvent.click(within(drawer()).getByRole("button", { name: locale.detail.restart.action }));
  await waitFor(() => assert.equal(screen.queryByText(refusal), null));
});

test("stopping keeps the drawer open and refetches it", async () => {
  stub("deploymentsPage", async () => listPage([deployment()]));
  let loads = 0;
  stub("deployment", async () => { loads += 1; return deployment(); });
  let stopped = 0;
  stub("stopDeployment", async () => { stopped += 1; });
  await openDetail();
  const before = loads;
  fireEvent.click(within(drawer()).getByRole("button", { name: locale.detail.stop }));
  const confirm = (await screen.findByText(locale.detail.stopConfirm)).closest(".ant-modal") as HTMLElement;
  fireEvent.click(within(confirm).getByRole("button", { name: locale.detail.stop }));
  await waitFor(() => assert.equal(stopped, 1));
  await waitFor(() => assert.ok(loads > before));
  assert.ok(drawer(), "the drawer is still open after stopping");
});

test("a stopped deployment offers only Start, which posts and refetches", async () => {
  const stoppedDep = deployment({ desired_state: "stopped", runtime_state: "stopped", instances: [] });
  stub("deploymentsPage", async () => listPage([stoppedDep]));
  let loads = 0;
  stub("deployment", async () => { loads += 1; return stoppedDep; });
  const calls: number[] = [];
  stub("startDeployment", async (id: number) => { calls.push(id); return deployment(); });
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/deployments?deployment=1"]}>
        <DeploymentsPage />
      </MemoryRouter>
    </I18nextProvider>,
  );
  const start = await screen.findByRole("button", { name: locale.detail.start });
  assert.equal(within(drawer()).queryByRole("button", { name: locale.detail.stop }), null);
  assert.equal(within(drawer()).queryByRole("button", { name: locale.detail.restart.action }), null);
  const before = loads;
  await userEvent.click(start);
  await waitFor(() => assert.deepEqual(calls, [1]));
  await waitFor(() => assert.ok(loads > before));
});

test("a start the server refuses reports why", async () => {
  const stoppedDep = deployment({ desired_state: "stopped", runtime_state: "stopped", instances: [] });
  stub("deploymentsPage", async () => listPage([stoppedDep]));
  stub("deployment", async () => stoppedDep);
  stub("startDeployment", async () => { throw new APIError("DEPLOYMENT_DELETING", {}, "the deployment is being deleted", 409); });
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/deployments?deployment=1"]}>
        <DeploymentsPage />
      </MemoryRouter>
    </I18nextProvider>,
  );
  fireEvent.click(await screen.findByRole("button", { name: locale.detail.start }));
  await screen.findByText(i18n.t("detail.startFailed", { detail: i18n.t("common:apiError.DEPLOYMENT_DELETING") }));
});

test("a deleting deployment cannot be restarted or stopped", async () => {
  const deleting = deployment({ desired_state: "deleting" });
  stub("deploymentsPage", async () => listPage([deleting]));
  stub("deployment", async () => deleting);
  const stop = await openDetail();
  assert.ok((stop as HTMLButtonElement).disabled);
  assert.ok((within(drawer()).getByRole("button", { name: locale.detail.restart.action }) as HTMLButtonElement).disabled);
});

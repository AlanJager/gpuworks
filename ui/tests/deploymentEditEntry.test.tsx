import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";

const { i18n, locale } = await createI18n(["deployments"], "en");
const { cleanup, render, screen } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const { default: DeploymentLifecycleActions, ReplicaRetryAlert } = await import("../src/pages/DeploymentLifecycleActions.tsx");
type Deployment = import("../src/api.ts").Deployment;

afterEach(cleanup);
after(() => dom.window.close());
const d = locale.deployments.detail;

function Probe() {
  const location = useLocation();
  return <div data-testid="route">{location.pathname + location.search}</div>;
}

function mount(node: React.ReactNode) {
  render(
    <MemoryRouter initialEntries={["/deployments?deployment=7"]}>
      <I18nextProvider i18n={i18n}>
        <Routes>
          <Route path="/deployments" element={node} />
          <Route path="*" element={<Probe />} />
        </Routes>
      </I18nextProvider>
    </MemoryRouter>,
  );
  return userEvent.setup({ document: dom.window.document });
}
const dep = (over: Partial<Deployment> = {}) => ({ id: 7, name: "qwen", desired_state: "active", replicas: 1, ...over }) as Deployment;
const route = () => screen.getByTestId("route").textContent;

test("Edit opens the deploy page on the deployment, Clone and modify on a copy of it", async () => {
  let user = mount(<DeploymentLifecycleActions dep={dep()} onChanged={() => {}} />);
  await user.click(screen.getByRole("button", { name: d.edit }));
  assert.equal(route(), "/deploy?edit=7");
  cleanup();
  user = mount(<DeploymentLifecycleActions dep={dep()} onChanged={() => {}} />);
  await user.click(screen.getByRole("button", { name: d.cloneModify }));
  assert.equal(route(), "/deploy?from=7");
});

test("a stopped deployment is edited too, next to its Start", () => {
  mount(<DeploymentLifecycleActions dep={dep({ desired_state: "stopped" })} onChanged={() => {}} />);
  assert.ok(screen.getByRole("button", { name: d.start }));
  assert.equal((screen.getByRole("button", { name: d.edit }) as HTMLButtonElement).disabled, false);
});

test("a deployment being deleted cannot be edited, only cloned", async () => {
  const user = mount(<DeploymentLifecycleActions dep={dep({ desired_state: "deleting" })} onChanged={() => {}} />);
  const edit = screen.getByRole("button", { name: d.edit }) as HTMLButtonElement;
  assert.equal(edit.disabled, true);
  await user.click(edit);
  assert.equal(screen.queryByTestId("route"), null, "the disabled button goes nowhere");
  await user.click(screen.getByRole("button", { name: d.cloneModify }));
  assert.equal(route(), "/deploy?from=7");
});

test("the give-up banner offers to fix the parameters, and the plain retry stays", async () => {
  const user = mount(
    <ReplicaRetryAlert dep={dep({ retry_exhausted: { index: 0, failed_attempts: 3, last_failure: "out of memory" } })} onChanged={() => {}} />,
  );
  assert.ok(screen.getByText(/out of memory/));
  assert.ok(screen.getByRole("button", { name: d.retry.action }));
  await user.click(screen.getByRole("button", { name: d.retry.editAndRetry }));
  assert.equal(route(), "/deploy?edit=7");
});

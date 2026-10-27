import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";

Object.defineProperty(dom.window, "CSS", { value: { escape: (text: string) => text }, configurable: true });
Object.defineProperty(globalThis, "KeyboardEvent", { value: dom.window.KeyboardEvent, configurable: true });

const { i18n, locale } = await createI18n(["deploy"], "en");
const { cleanup, render, screen, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { ConfigProvider } = await import("antd");
const { default: DeployEditConfirmModal } = await import("../src/pages/deploy/DeployEditConfirmModal.tsx");
const { APIError } = await import("../src/apiError.ts");
type Review = import("../src/pages/deploy/useDeployEditSubmit.ts").EditReview;
type Plan = import("../src/api.ts").DeploymentEditPlan;
type Choice = import("../src/pages/deploy/useDeployEditSubmit.ts").EditChoice;

afterEach(cleanup);
after(() => dom.window.close());
const edit = locale.deploy.edit;

const plan = (over: Partial<Plan> = {}): Plan => ({
  changes: [
    { field: "quick_params.max_model_len", layer: "restart", from: "4096", to: "8192" },
    { field: "replicas", layer: "live", from: "2", to: "" },
    { field: "container_overrides.env.TOKEN", layer: "restart", from: "", to: "", masked: true },
  ],
  restart: true, apply_options: ["rolling", "all", "none"], default_apply: "rolling", can_start: false, can_wake: false,
  revision: "r1", ...over,
});

function mount(review: Review, handlers: { save?: (choice: Choice) => void; reload?: () => void; cancel?: () => void } = {}) {
  const saves: Choice[] = [];
  let reloads = 0;
  let cancels = 0;
  render(
    <I18nextProvider i18n={i18n}>
      <ConfigProvider theme={{ token: { motion: false } }}>
        <DeployEditConfirmModal
          review={review} saving={false} reloading={false}
          onCancel={() => { cancels++; }} onSave={(choice) => saves.push(choice)} onReload={() => { reloads++; }}
        />
      </ConfigProvider>
    </I18nextProvider>,
  );
  return { saves, reloads: () => reloads, cancels: () => cancels, user: userEvent.setup({ document: dom.window.document }) };
}

test("each change shows its field, old and new value and when it takes effect; absent values are a dash and secrets are masked", async () => {
  mount({ plan: plan(), body: {} });
  const dialog = await screen.findByRole("dialog");
  const rows = within(dialog).getAllByRole("row").slice(1).map((row) => within(row).getAllByRole("cell").map((cell) => cell.textContent));
  assert.deepEqual(rows, [
    ["quick_params.max_model_len", "4096", "8192", edit.layer.restart],
    ["replicas", "2", "—", edit.layer.live],
    ["container_overrides.env.TOKEN", "••••", "••••", edit.layer.restart],
  ]);
});

test("the server's default apply is preselected and the primary button names the choice", async () => {
  const { saves, user } = mount({ plan: plan(), body: {} });
  const dialog = await screen.findByRole("dialog");
  const checked = () => within(dialog).getAllByRole("radio").filter((radio) => (radio as HTMLInputElement).checked).map((radio) => radio.getAttribute("value"));
  assert.deepEqual(checked(), ["rolling"]);
  await user.click(within(dialog).getByRole("button", { name: edit.ok.rolling }));
  assert.deepEqual(saves, [{ apply: "rolling" }]);
  for (const [value, label] of [["all", edit.ok.all], ["none", edit.ok.save]] as const) {
    await user.click(within(dialog).getByRole("radio", { name: new RegExp(`^${locale.deploy.edit.apply[value].label}`) }));
    assert.ok(within(dialog).getByRole("button", { name: label }), `${value} reads ${label}`);
  }
});

test("a partition change says why it cannot roll: all at once is the default and rolling is not offered", async () => {
  const { saves, user } = mount({ plan: plan({ apply_options: ["all", "none"], default_apply: "all", apply_note: "partition_change" }), body: {} });
  const dialog = await screen.findByRole("dialog");
  assert.ok(within(dialog).getByText(edit.applyNote.partition_change));
  assert.deepEqual(within(dialog).getAllByRole("radio").map((radio) => radio.getAttribute("value")), ["all", "none"]);
  await user.click(within(dialog).getByRole("button", { name: edit.ok.all }));
  assert.deepEqual(saves, [{ apply: "all" }]);
});

test("a plan without a note shows none", async () => {
  mount({ plan: plan(), body: {} });
  const dialog = await screen.findByRole("dialog");
  assert.equal(within(dialog).queryByText(edit.applyNote.partition_change), null);
});

test("a stopped deployment saves only, or starts with the edit when asked", async () => {
  const { saves, user } = mount({ plan: plan({ apply_options: ["none"], default_apply: "none", can_start: true }), body: {} });
  const dialog = await screen.findByRole("dialog");
  await user.click(within(dialog).getByRole("button", { name: edit.ok.save }));
  await user.click(within(dialog).getByRole("checkbox", { name: edit.confirm.startAfter }));
  await user.click(within(dialog).getByRole("button", { name: edit.ok.startSave }));
  assert.deepEqual(saves, [{ apply: "none" }, { apply: "none", start: true }]);
});

test("a sleeping deployment can wake and apply, which is all instances replaced awake", async () => {
  const { saves, user } = mount({ plan: plan({ apply_options: ["none"], default_apply: "none", can_wake: true }), body: {} });
  const dialog = await screen.findByRole("dialog");
  await user.click(within(dialog).getByRole("radio", { name: new RegExp(`^${edit.apply.wake.label}`) }));
  await user.click(within(dialog).getByRole("button", { name: edit.ok.wake }));
  assert.deepEqual(saves, [{ apply: "all", wake: true }]);
});

test("an edit that restarts nothing has no apply choice, and nothing to apply sends none", async () => {
  const { saves, user } = mount({ plan: plan({ restart: false, apply_options: [], default_apply: "none", changes: [{ field: "replicas", layer: "live", from: "2", to: "3" }] }), body: {} });
  const dialog = await screen.findByRole("dialog");
  assert.equal(within(dialog).queryAllByRole("radio").length, 0);
  await user.click(within(dialog).getByRole("button", { name: edit.ok.save }));
  assert.deepEqual(saves, [{}]);
});

test("a plan without changes says so and cannot be saved", async () => {
  mount({ plan: plan({ changes: [] }), body: {} });
  const dialog = await screen.findByRole("dialog");
  assert.ok(within(dialog).getByText(edit.confirm.noChanges));
  assert.equal((within(dialog).getByRole("button", { name: edit.ok.rolling }) as HTMLButtonElement).disabled, true);
});

test("a deployment changed under the edit offers a reload and cannot save the stale plan", async () => {
  const { reloads, user } = mount({ plan: plan(), error: new APIError("DEPLOYMENT_CHANGED", { revision: "r2" }, "changed", 409), body: {} });
  const dialog = await screen.findByRole("dialog");
  assert.ok(within(dialog).getByText(edit.confirm.conflict));
  assert.equal((within(dialog).getByRole("button", { name: edit.ok.rolling }) as HTMLButtonElement).disabled, true, "the plan is stale");
  await user.click(within(dialog).getByRole("button", { name: edit.confirm.reload }));
  assert.equal(reloads(), 1);
});

test("a conflict found by the dry run has no plan yet and the same way out", async () => {
  mount({ error: new APIError("DEPLOYMENT_CHANGED", {}, "changed", 409), body: {} });
  const dialog = await screen.findByRole("dialog");
  assert.ok(within(dialog).getByRole("button", { name: edit.confirm.reload }));
  assert.equal(within(dialog).queryAllByRole("radio").length, 0);
});

test("cancel is a button the keyboard reaches", async () => {
  const { cancels, user } = mount({ plan: plan(), body: {} });
  const dialog = await screen.findByRole("dialog");
  within(dialog).getByRole("button", { name: edit.confirm.cancel }).focus();
  await user.keyboard("{Enter}");
  assert.equal(cancels(), 1);
});

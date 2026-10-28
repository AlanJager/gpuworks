import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React, { useState } from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// A deployment's scheduling policy card: one summary line over the three
// policies (rescheduling on node loss, schedule, idle sleep), and the dialog
// that edits them. The editors' own validation is in deploymentPolicyEditors.

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["deployments", "deploy"]);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { formatTime } = await import("../src/i18n/datetime.ts");
const { default: DeploymentSchedulingPolicy } = await import("../src/pages/DeploymentSchedulingPolicy.tsx");
type Deployment = import("../src/api.ts").Deployment;
type Schedule = import("../src/api.ts").LifecycleSchedule;

const p = locale.deployments.schedulingPolicy;
const a = locale.deployments.automation;
const n = locale.deployments.nodeReplacement;
const original = { ...api };

function deployment(overrides: Record<string, unknown> = {}): Deployment {
  return {
    id: 1, name: "qwen", model_id: 1, model_name: "m", served_name: "m", engine: "vllm", node_uuid: "n1",
    desired_state: "active", runtime_state: "running", sleepable: true, replicas: 2, max_replicas: 32, nnodes: 1,
    health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] },
    template_deviated: false, tokenize_enabled: false, created_at: "2026-10-01T00:00:00Z",
    idle_sleep: { after_sec: 0, level: 2 }, node_replacement: { after_sec: 600 },
    ...overrides,
  } as unknown as Deployment;
}

const nightly: Schedule = { id: 5, action: "sleep", weekdays: [1, 2, 3, 4, 5], time: "22:00", timezone: "", sleep_level: 2, enabled: true, next_at: "2026-10-30T14:00:00Z" };
const morning: Schedule = { id: 6, action: "wake", weekdays: [1, 2, 3, 4, 5], time: "08:30", timezone: "", enabled: true, next_at: "2026-10-31T00:30:00Z" };
const paused: Schedule = { id: 7, action: "wake", weekdays: [0], time: "01:00", timezone: "", enabled: false, next_at: "2026-10-29T01:00:00Z" };

// What the server holds: the stubs write here and the host reloads from it, as
// the deployment page does after onChanged.
let current: Deployment;
let schedules: Schedule[];
let role = "admin";
let changed = 0;

function stub() {
  schedules = [];
  role = "admin";
  changed = 0;
  api.me = async () => ({ id: 1, username: "u", role }) as Awaited<ReturnType<typeof api.me>>;
  api.deploymentSchedule = async () => ({ items: schedules, total: schedules.length });
  api.createSchedule = async (_id, body) => {
    const row = { ...body, id: 20 + schedules.length, enabled: body.enabled ?? true, next_at: "2026-10-30T15:00:00Z" } as Schedule;
    schedules = [...schedules, row];
    return row;
  };
  api.setIdleSleep = async (_id, body) => { current = { ...current, idle_sleep: body }; return body; };
  api.setNodeReplacement = async (_id, body) => { current = { ...current, node_replacement: body }; return body; };
}

function Host() {
  const [dep, setDep] = useState(current);
  return <DeploymentSchedulingPolicy dep={dep} onChanged={() => { changed += 1; setDep(current); }} />;
}

function mount(dep = deployment()) {
  current = dep;
  render(<I18nextProvider i18n={i18n}><Host /></I18nextProvider>);
  return userEvent.setup();
}

const item = (label: string, value: string) => i18n.t("deployments:schedulingPolicy.item", { label, value });
const line = (...items: string[]) => items.join(p.separator);
const failover = (value: string) => item(n.title, value);
const schedule = (value: string) => item(a.schedule.title, value);
const idle = (value: string) => item(a.idle.title, value);
const labelled = (scope: ReturnType<typeof within>, text: string) => scope.getByText(text).closest("button") as HTMLButtonElement;
const dialogs = () => screen.getAllByRole("dialog");

afterEach(() => { cleanup(); Object.assign(api, original); });
after(() => closeChannels());

test("collapsed, the card sums up node-loss rescheduling, the schedule and idle sleep in one line", async () => {
  stub();
  mount();
  await screen.findByText(line(failover("10 min"), schedule(p.none), idle(p.off)));
  assert.ok(screen.getByRole("region", { name: p.title }));
});

test("the schedule part counts the orders and names the next enabled one, with what it does", async () => {
  stub();
  schedules = [paused, morning, nightly];
  mount();
  const count = i18n.t("deployments:schedulingPolicy.scheduleCount", { count: 3 });
  const next = i18n.t("deployments:schedulingPolicy.scheduleNext", {
    time: formatTime(nightly.next_at),
    order: i18n.t("deployments:automation.schedule.actionText.sleep", { level: 2 }),
  });
  await screen.findByText(line(failover("10 min"), schedule(count + next), idle(p.off)));
  cleanup();

  schedules = [paused];
  mount();
  await screen.findByText(line(failover("10 min"), schedule(i18n.t("deployments:schedulingPolicy.scheduleCount", { count: 1 }) + p.scheduleAllOff), idle(p.off)));
});

test("rescheduling on node loss reads Off at 0, and long waits read in hours and days", async () => {
  stub();
  mount(deployment({ node_replacement: { after_sec: 0 } }));
  await screen.findByText(line(failover(p.off), schedule(p.none), idle(p.off)));
  cleanup();
  mount(deployment({ node_replacement: { after_sec: 7200 } }));
  await screen.findByText(line(failover("2 h"), schedule(p.none), idle(p.off)));
  cleanup();
  // A deployment with no saved setting waits the default 10 minutes.
  mount(deployment({ node_replacement: undefined }));
  await screen.findByText(line(failover("10 min"), schedule(p.none), idle(p.off)));
});

test("idle sleep shows its wait and level; a deployment that cannot sleep has no idle sleep to show", async () => {
  stub();
  mount(deployment({ idle_sleep: { after_sec: 900, level: 2 } }));
  await screen.findByText(line(failover("10 min"), schedule(p.none), idle("15 min / L2")));
  cleanup();
  mount(deployment({ idle_sleep: { after_sec: 3600, level: 1 } }));
  await screen.findByText(line(failover("10 min"), schedule(p.none), idle("60 min / L1")));
  cleanup();
  mount(deployment({ sleepable: false }));
  await screen.findByText(line(failover("10 min"), schedule(p.none)));
  assert.equal(screen.queryByText(new RegExp(a.idle.title)), null);
});

test("the schedule part says it is loading, and that it failed when the list cannot be read", async () => {
  stub();
  api.deploymentSchedule = () => new Promise(() => {});
  mount();
  await screen.findByText(line(failover("10 min"), schedule(p.loading), idle(p.off)));
  cleanup();
  api.deploymentSchedule = async () => { throw new APIError("DEPLOYMENT_NOT_FOUND", {}, "no such deployment", 404); };
  mount();
  await screen.findByText(line(failover("10 min"), schedule(p.loadFailed), idle(p.off)));
});

test("a read-only user sees the summary and no edit action; so does the card of a deployment being deleted", async () => {
  stub();
  role = "viewer";
  mount();
  await screen.findByText(line(failover("10 min"), schedule(p.none), idle(p.off)));
  await waitFor(() => assert.equal(screen.queryByRole("button", { name: p.edit }), null));
  cleanup();
  role = "admin";
  mount(deployment({ desired_state: "deleting" }));
  assert.equal(screen.queryByRole("region", { name: p.title }), null);
  assert.equal(screen.queryByText(p.title), null);
});

test("the edit action opens the dialog from the keyboard, with a tab for each policy", async () => {
  stub();
  const user = mount();
  const edit = await screen.findByRole("button", { name: p.edit });
  edit.focus();
  await user.keyboard("{Enter}");
  const dialog = within(await screen.findByRole("dialog"));
  const tabs = dialog.getAllByRole("tab").map((tab) => tab.textContent);
  assert.deepEqual(tabs, [n.title, a.schedule.title, a.idle.title]);
  // The long explanations are not on the page any more.
  assert.equal(screen.queryByText(/回来|returns before the moved replica/), null);
  fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape", keyCode: 27 });
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  edit.focus();
  await user.keyboard(" ");
  await screen.findByRole("dialog");
  await user.click(screen.getByRole("button", { name: p.close }));
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  cleanup();

  const user2 = mount(deployment({ sleepable: false }));
  await user2.click(await screen.findByRole("button", { name: p.edit }));
  const plain = within(await screen.findByRole("dialog"));
  assert.deepEqual(plain.getAllByRole("tab").map((tab) => tab.textContent), [n.title, a.schedule.title]);
});

test("saving the wait on the first tab calls the node replacement API and updates the summary", async () => {
  stub();
  const saved: unknown[] = [];
  const set = api.setNodeReplacement;
  api.setNodeReplacement = async (id, body) => { saved.push(body); return set(id, body); };
  const user = mount();
  await user.click(await screen.findByRole("button", { name: p.edit }));
  const dialog = within(await screen.findByRole("dialog"));
  fireEvent.change(dialog.getByLabelText(n.minutes), { target: { value: "30" } });
  await user.click(labelled(dialog, n.save));
  await waitFor(() => assert.deepEqual(saved, [{ after_sec: 1800 }]));
  await screen.findByText(n.saved);
  assert.equal(changed, 1);
  await screen.findByText(line(failover("30 min"), schedule(p.none), idle(p.off)));
  // Turning it off.
  fireEvent.change(dialog.getByLabelText(n.minutes), { target: { value: "0" } });
  await user.click(labelled(dialog, n.save));
  await screen.findByText(line(failover(p.off), schedule(p.none), idle(p.off)));
});

test("saving idle sleep on its tab calls the idle sleep API and updates the summary", async () => {
  stub();
  const saved: unknown[] = [];
  const set = api.setIdleSleep;
  api.setIdleSleep = async (id, body) => { saved.push(body); return set(id, body); };
  const user = mount();
  await user.click(await screen.findByRole("button", { name: p.edit }));
  await user.click(within(await screen.findByRole("dialog")).getByRole("tab", { name: a.idle.title }));
  const section = within(await screen.findByRole("region", { name: a.idle.title }));
  await user.click(section.getByRole("switch", { name: a.idle.enable }));
  await user.click(labelled(section, a.save));
  await waitFor(() => assert.deepEqual(saved, [{ after_sec: 900, level: 2 }]));
  await screen.findByText(line(failover("10 min"), schedule(p.none), idle("15 min / L2")));
  assert.equal(changed, 1);
});

test("adding an order on the schedule tab reloads the list and the summary", async () => {
  stub();
  const user = mount();
  await user.click(await screen.findByRole("button", { name: p.edit }));
  await user.click(within(await screen.findByRole("dialog")).getByRole("tab", { name: a.schedule.title }));
  const section = within(await screen.findByRole("region", { name: a.schedule.title }));
  await section.findByText(a.schedule.empty);
  await user.click(section.getByRole("button", { name: a.schedule.add }));
  const form = within(dialogs().at(-1) as HTMLElement);
  fireEvent.change(form.getByLabelText(a.schedule.field.time), { target: { value: "23:30" } });
  await user.click(form.getByRole("button", { name: a.schedule.save }));
  await section.findByText(`23:30 ${a.schedule.serverZone}`);
  const next = i18n.t("deployments:schedulingPolicy.scheduleNext", {
    time: formatTime("2026-10-30T15:00:00Z"),
    order: i18n.t("deployments:automation.schedule.actionText.sleep", { level: 2 }),
  });
  await screen.findByText(line(failover("10 min"), schedule(i18n.t("deployments:schedulingPolicy.scheduleCount", { count: 1 }) + next), idle(p.off)));
});

test("a refused save is shown and leaves the summary as it was", async () => {
  stub();
  api.setNodeReplacement = async () => { throw new APIError("NODE_REPLACEMENT_INVALID", { minimum: 60, maximum: 604800 }, "out of range", 400); };
  api.setIdleSleep = async () => { throw new APIError("SLEEP_NOT_ENABLED", {}, "sleep mode was not enabled", 400); };
  const user = mount();
  await user.click(await screen.findByRole("button", { name: p.edit }));
  const dialog = within(await screen.findByRole("dialog"));
  fireEvent.change(dialog.getByLabelText(n.minutes), { target: { value: "0.5" } });
  fireEvent.change(dialog.getByLabelText(n.minutes), { target: { value: "1" } });
  await user.click(labelled(dialog, n.save));
  await screen.findByText(i18n.t("deployments:nodeReplacement.saveFailed", { detail: i18n.t("common:apiError.NODE_REPLACEMENT_INVALID", { minimum: 60, maximum: 604800 }) }));
  await user.click(dialog.getByRole("tab", { name: a.idle.title }));
  const section = within(await screen.findByRole("region", { name: a.idle.title }));
  await user.click(section.getByRole("switch", { name: a.idle.enable }));
  await user.click(labelled(section, a.save));
  await screen.findByText(i18n.t("deployments:automation.idle.saveFailed", { detail: i18n.t("common:apiError.SLEEP_NOT_ENABLED") }));
  assert.equal(changed, 0);
  assert.ok(screen.getByText(line(failover("10 min"), schedule(p.none), idle(p.off))));
});

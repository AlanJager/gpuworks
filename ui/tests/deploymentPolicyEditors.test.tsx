import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import * as shapes from "./helpers/deploymentCapabilities.ts";

// The editors behind a deployment's scheduling policy card: the schedule list
// and its dialog (create, edit, delete, validation, the server's refusals) and
// the wait before a replica leaves a lost node. The yield policy editor is in
// deploymentYieldPolicy.test.tsx, the card that holds them all in
// deploymentSchedulingPolicy.test.tsx.

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["deployments", "deploy"]);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { formatTime } = await import("../src/i18n/datetime.ts");
const { default: ScheduleSection, useSchedules } = await import("../src/pages/DeploymentSchedule.tsx");
const { default: NodeReplacementEditor } = await import("../src/pages/DeploymentNodeReplacement.tsx");
type Deployment = import("../src/api.ts").Deployment;
type Schedule = import("../src/api.ts").LifecycleSchedule;
type Request = import("../src/api.ts").LifecycleScheduleRequest;

const a = locale.deployments.automation;
const n = locale.deployments.nodeReplacement;
const original = { ...api };

function deployment(overrides: Record<string, unknown> = {}): Deployment {
  return {
    id: 1, name: "qwen", model_id: 1, model_name: "m", served_name: "m", engine: "vllm", node_uuid: "n1",
    desired_state: "active", runtime_state: "running", sleepable: true, replicas: 2, max_replicas: 32, nnodes: 1,
    health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] },
    template_deviated: false, tokenize_enabled: false, created_at: "2026-10-01T00:00:00Z",
    node_replacement: { after_sec: 600 },
    ...shapes.sleepableSingleNode,
    ...overrides,
  } as unknown as Deployment;
}

const nightly: Schedule = { id: 5, action: "sleep", weekdays: [1, 2, 3, 4, 5], time: "22:00", timezone: "", sleep_level: 2, enabled: true, next_at: "2026-10-26T14:00:00Z" };

let schedules: Schedule[] = [];
const created: Request[] = [];
const updated: Array<[number, Request]> = [];
const deleted: number[] = [];

function stub() {
  schedules = [];
  created.length = updated.length = deleted.length = 0;
  api.deploymentSchedule = async () => ({ items: schedules, total: schedules.length });
  api.createSchedule = async (_id, body) => {
    created.push(body);
    const row = { ...body, id: 10 + created.length, enabled: body.enabled ?? true } as Schedule;
    schedules = [...schedules, row];
    return row;
  };
  api.updateSchedule = async (_id, sid, body) => {
    updated.push([sid, body]);
    const row = { ...body, id: sid, enabled: body.enabled ?? true } as Schedule;
    schedules = schedules.map((item) => (item.id === sid ? row : item));
    return row;
  };
  api.deleteSchedule = async (_id, sid) => { deleted.push(sid); schedules = schedules.filter((item) => item.id !== sid); };
}

function Schedules({ dep }: { dep: Deployment }) {
  const schedules = useSchedules(dep.id);
  return <ScheduleSection dep={dep} schedules={schedules} />;
}

function mount(dep = deployment()) {
  render(
    <I18nextProvider i18n={i18n}>
      <Schedules dep={dep} />
    </I18nextProvider>,
  );
  return userEvent.setup();
}

function mountFailover(dep = deployment(), onChanged = () => {}) {
  render(<I18nextProvider i18n={i18n}><NodeReplacementEditor dep={dep} onChanged={onChanged} /></I18nextProvider>);
  return userEvent.setup();
}

// A button found by its label: after an order the loading icon's leave motion
// never ends in jsdom and stays in the button's accessible name.
const labelled = (scope: ReturnType<typeof within>, text: string) => scope.getByText(text).closest("button") as HTMLButtonElement;

const scheduleSection = () => screen.getByRole("region", { name: a.schedule.title });
const dialog = () => screen.getByRole("dialog");

afterEach(() => { cleanup(); Object.assign(api, original); });
after(() => closeChannels());

test("the schedule lists each order with its days, time zone and next run; none says so", async () => {
  stub();
  mount();
  await within(scheduleSection()).findByText(a.schedule.empty);
  cleanup();

  schedules = [nightly, { id: 6, action: "wake", weekdays: [1, 2, 3, 4, 5, 6, 0], time: "08:30", timezone: "Asia/Shanghai", enabled: false }];
  mount();
  const section = scheduleSection();
  await within(section).findByText(i18n.t("deployments:automation.schedule.actionText.sleep", { level: 2 }));
  assert.ok(within(section).getByText("Mon Tue Wed Thu Fri"));
  assert.ok(within(section).getByText(`22:00 ${a.schedule.serverZone}`));
  assert.ok(within(section).getByText(formatTime("2026-10-26T14:00:00Z")));
  assert.ok(within(section).getByText(a.schedule.everyDay));
  assert.ok(within(section).getByText("08:30 Asia/Shanghai"));
  // A disabled order has no next run.
  const row = within(section).getByText(a.schedule.everyDay).closest("tr") as HTMLElement;
  assert.ok(within(row).getByText("—"));
});

test("a failed schedule load shows the reason and retries", async () => {
  stub();
  let calls = 0;
  api.deploymentSchedule = async () => {
    calls += 1;
    if (calls === 1) throw new APIError("DEPLOYMENT_NOT_FOUND", {}, "no such deployment", 404);
    return { items: [nightly], total: 1 };
  };
  const user = mount();
  await within(scheduleSection()).findByText(/no such deployment|DEPLOYMENT_NOT_FOUND/);
  await user.click(within(scheduleSection()).getByRole("button", { name: a.retry }));
  await within(scheduleSection()).findByText("Mon Tue Wed Thu Fri");
});

test("adding an order sends the days, time and zone it was given, then lists it", async () => {
  stub();
  const user = mount();
  await within(scheduleSection()).findByText(a.schedule.empty);
  await user.click(within(scheduleSection()).getByRole("button", { name: a.schedule.add }));
  const modal = within(dialog());
  // Weekdays start Monday to Friday; drop Friday and add Saturday.
  const toggle = (name: string) => modal.getByRole("button", { name });
  assert.equal(toggle("Fri").getAttribute("aria-pressed"), "true");
  assert.equal(toggle("Sat").getAttribute("aria-pressed"), "false");
  await user.click(toggle("Fri"));
  await user.click(toggle("Sat"));
  fireEvent.change(modal.getByLabelText(a.schedule.field.time), { target: { value: "23:30" } });
  await user.type(modal.getByLabelText(a.schedule.field.timezone), "Asia/Shanghai");
  await user.click(modal.getByRole("button", { name: a.schedule.save }));
  await waitFor(() => assert.deepEqual(created, [
    { action: "sleep", weekdays: [1, 2, 3, 4, 6], time: "23:30", timezone: "Asia/Shanghai", sleep_level: 2, enabled: true },
  ]));
  await within(scheduleSection()).findByText("23:30 Asia/Shanghai");
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
});

test("an order the form can see is wrong is not sent: no day, no time", async () => {
  stub();
  const user = mount();
  await within(scheduleSection()).findByText(a.schedule.empty);
  await user.click(within(scheduleSection()).getByRole("button", { name: a.schedule.add }));
  const modal = within(dialog());
  await user.click(modal.getByRole("button", { name: a.schedule.save }));
  await modal.findByText(a.schedule.errors.time);
  for (const day of ["Mon", "Tue", "Wed", "Thu", "Fri"]) await user.click(modal.getByRole("button", { name: day }));
  fireEvent.change(modal.getByLabelText(a.schedule.field.time), { target: { value: "07:00" } });
  await user.click(modal.getByRole("button", { name: a.schedule.save }));
  await modal.findByText(a.schedule.errors.weekdays);
  assert.deepEqual(created, []);
});

test("what the server still refuses shows in the dialog: an invalid order names its field, an unknown zone its name", async () => {
  stub();
  const user = mount();
  await within(scheduleSection()).findByText(a.schedule.empty);
  await user.click(within(scheduleSection()).getByRole("button", { name: a.schedule.add }));
  const modal = within(dialog());
  fireEvent.change(modal.getByLabelText(a.schedule.field.time), { target: { value: "07:00" } });
  api.createSchedule = async () => {
    throw new APIError("SCHEDULE_INVALID", { field: "time", reason: "use HH:MM, 00:00 to 23:59" }, "schedule time: use HH:MM, 00:00 to 23:59", 400);
  };
  await user.click(labelled(modal, a.schedule.save));
  await modal.findByText(i18n.t("common:apiError.SCHEDULE_INVALID", { field: a.schedule.field.time, reason: "use HH:MM, 00:00 to 23:59" }));
  api.createSchedule = async () => {
    throw new APIError("SCHEDULE_TIMEZONE_UNKNOWN", { timezone: "Mars/Base" }, "unknown zone", 400);
  };
  await user.type(modal.getByLabelText(a.schedule.field.timezone), "Mars/Base");
  await user.click(labelled(modal, a.schedule.save));
  await modal.findByText(i18n.t("common:apiError.SCHEDULE_TIMEZONE_UNKNOWN", { timezone: "Mars/Base" }));
  assert.equal(modal.queryByText(/use HH:MM/), null);
  // The dialog stays open, and the first refusal's text is gone.
  assert.ok(screen.getByRole("dialog"));
});

test("editing an order replaces it; the switch enables and disables it", async () => {
  stub();
  schedules = [nightly];
  const user = mount();
  const section = scheduleSection();
  await within(section).findByText("Mon Tue Wed Thu Fri");
  await user.click(within(section).getByRole("button", { name: a.schedule.edit }));
  const modal = within(dialog());
  assert.equal((modal.getByLabelText(a.schedule.field.time) as HTMLInputElement).value, "22:00");
  fireEvent.change(modal.getByLabelText(a.schedule.field.time), { target: { value: "21:15" } });
  await user.click(modal.getByRole("button", { name: a.schedule.save }));
  await waitFor(() => assert.deepEqual(updated, [[5, { action: "sleep", weekdays: [1, 2, 3, 4, 5], time: "21:15", timezone: "", sleep_level: 2, enabled: true }]]));
  await within(section).findByText(`21:15 ${a.schedule.serverZone}`);
  await user.click(within(section).getByRole("switch", { name: i18n.t("deployments:automation.schedule.enabledOf", { order: i18n.t("deployments:automation.schedule.actionText.sleep", { level: 2 }), time: "21:15" }) }));
  await waitFor(() => assert.equal(updated.at(-1)?.[1].enabled, false));
});

test("deleting an order asks first, then removes it", async () => {
  stub();
  schedules = [nightly];
  const user = mount();
  const section = scheduleSection();
  await within(section).findByText("Mon Tue Wed Thu Fri");
  await user.click(within(section).getByRole("button", { name: a.schedule.delete }));
  assert.deepEqual(deleted, []);
  const confirm = await screen.findByText(a.schedule.deleteConfirm);
  await user.click(within(confirm.closest(".ant-popover") as HTMLElement).getByRole("button", { name: a.schedule.delete }));
  await waitFor(() => assert.deepEqual(deleted, [5]));
  await within(section).findByText(a.schedule.empty);
});

test("the schedule offers the orders the server allows: sleep and wake where it can sleep, a replica count where its replicas are placed for it", async () => {
  stub();
  // Not sleepable: the only order is a replica count.
  let user = mount(deployment({ sleepable: false, ...shapes.notSleepableSingleNode }));
  await within(scheduleSection()).findByText(a.schedule.empty);
  await user.click(within(scheduleSection()).getByRole("button", { name: a.schedule.add }));
  let modal = within(dialog());
  assert.ok(modal.getByLabelText(a.schedule.field.replicas));
  assert.equal(modal.queryByLabelText(a.schedule.field.sleep_level), null);
  assert.ok(modal.getByText(a.schedule.actionName.replicas));
  cleanup();

  // A sleepable multi-node deployment sleeps and wakes but its replica count is not scheduled.
  user = mount(deployment({ nnodes: 2, ...shapes.sleepableMultiNodeManual }));
  await within(scheduleSection()).findByText(a.schedule.empty);
  await user.click(within(scheduleSection()).getByRole("button", { name: a.schedule.add }));
  modal = within(dialog());
  assert.ok(modal.getByRole("combobox", { name: a.schedule.field.sleep_level }));
  fireEvent.mouseDown(modal.getByRole("combobox", { name: a.schedule.field.action }));
  const options = (await screen.findAllByRole("option")).map((option) => option.getAttribute("aria-label") ?? option.textContent);
  assert.deepEqual(options, [a.schedule.actionName.sleep, a.schedule.actionName.wake]);
  cleanup();

  // Neither: adding is disabled and says why.
  mount(deployment({ sleepable: false, nnodes: 2, ...shapes.notSleepableMultiNodeManual }));
  await within(scheduleSection()).findByText(a.schedule.noActions);
  assert.equal((within(scheduleSection()).getByRole("button", { name: a.schedule.add }) as HTMLButtonElement).disabled, true);
});

test("the wait before a replica leaves a lost node starts at 10 minutes and saves seconds; 0 turns it off", async () => {
  const saved: unknown[] = [];
  let changed = 0;
  api.setNodeReplacement = async (_id, body) => { saved.push(body); return body; };
  const user = mountFailover(deployment({ node_replacement: undefined }), () => { changed++; });
  const section = screen.getByRole("region", { name: n.title });
  const minutes = within(section).getByLabelText(n.minutes) as HTMLInputElement;
  assert.equal(minutes.value, "10");
  assert.ok(within(section).getByText(n.zeroNever));
  assert.equal((within(section).getByRole("button", { name: n.save }) as HTMLButtonElement).disabled, true);
  fireEvent.change(minutes, { target: { value: "20" } });
  await user.click(labelled(within(section), n.save));
  await waitFor(() => assert.deepEqual(saved, [{ after_sec: 1200 }]));
  await screen.findByText(n.saved);
  assert.equal(changed, 1);
  fireEvent.change(minutes, { target: { value: "0" } });
  await user.click(labelled(within(section), n.save));
  await waitFor(() => assert.deepEqual(saved.at(-1), { after_sec: 0 }));
});

test("a saved wait is shown as saved, 0 reads as off, and saving needs a change", () => {
  mountFailover(deployment({ node_replacement: { after_sec: 0 } }));
  const section = screen.getByRole("region", { name: n.title });
  assert.equal((within(section).getByLabelText(n.minutes) as HTMLInputElement).value, "0");
  assert.equal((within(section).getByRole("button", { name: n.save }) as HTMLButtonElement).disabled, true);
  cleanup();
  mountFailover(deployment({ node_replacement: { after_sec: 5400 } }));
  assert.equal((within(screen.getByRole("region", { name: n.title })).getByLabelText(n.minutes) as HTMLInputElement).value, "90");
});

test("a refused wait says why, in the words of the node-loss rescheduling", async () => {
  api.setNodeReplacement = async () => { throw new APIError("NODE_REPLACEMENT_INVALID", { minimum: 60, maximum: 604800 }, "out of range", 400); };
  const user = mountFailover(deployment({ nnodes: 2, ...shapes.sleepableMultiNodeManual }));
  const section = screen.getByRole("region", { name: n.title });
  fireEvent.change(within(section).getByLabelText(n.minutes), { target: { value: "1.5" } });
  fireEvent.change(within(section).getByLabelText(n.minutes), { target: { value: "5" } });
  await user.click(within(section).getByRole("button", { name: n.save }));
  await screen.findByText(i18n.t("deployments:nodeReplacement.saveFailed", { detail: i18n.t("common:apiError.NODE_REPLACEMENT_INVALID", { minimum: 60, maximum: 604800 }) }));
  assert.match(i18n.t("common:apiError.NODE_REPLACEMENT_INVALID", { minimum: 60, maximum: 604800 }), /节点失联重调度|node-loss rescheduling/);
});

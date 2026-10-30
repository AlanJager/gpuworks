import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// A PD deployment's replica count is per role: the schedule editor asks for the
// role, bounds the count by what the mode allows that role (prefill may be 0),
// and the list and the scheduling policy summary say which role an order sets.

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["deployments", "deploy"]);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: ScheduleSection, useSchedules } = await import("../src/pages/DeploymentSchedule.tsx");
const { default: DeploymentSchedulingPolicy } = await import("../src/pages/DeploymentSchedulingPolicy.tsx");
type Deployment = import("../src/api.ts").Deployment;
type Schedule = import("../src/api.ts").LifecycleSchedule;
type Request = import("../src/api.ts").LifecycleScheduleRequest;

const a = locale.deployments.automation;
const original = { ...api };

function pdDeployment(overrides: Record<string, unknown> = {}): Deployment {
  return {
    id: 3, name: "pd-chat", model_id: 1, model_name: "m", served_name: "m", engine: "vllm", node_uuid: "n1",
    desired_state: "active", runtime_state: "running", sleepable: false, replicas: 1, max_replicas: 4, nnodes: 1,
    placement_mode: "auto",
    pd: {
      mode: "vllm-nixl", fallback: "", kv_load_failure: "", state: "running", short: false,
      roles: [
        { role: "decode", tp: 1, desired: 2, ready: 2, may_be_empty: false, units: [] },
        { role: "prefill", tp: 1, desired: 1, ready: 1, may_be_empty: true, units: [] },
      ],
    },
    health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] },
    template_deviated: false, tokenize_enabled: false, created_at: "2026-10-01T00:00:00Z",
    node_replacement: { after_sec: 600 },
    ...overrides,
  } as unknown as Deployment;
}

let schedules: Schedule[] = [];
const created: Request[] = [];
const updated: Array<[number, Request]> = [];

function stub() {
  schedules = [];
  created.length = updated.length = 0;
  api.me = async () => ({ id: 1, username: "u", role: "admin" }) as Awaited<ReturnType<typeof api.me>>;
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
}

function Schedules({ dep }: { dep: Deployment }) {
  const loaded = useSchedules(dep.id);
  return <ScheduleSection dep={dep} schedules={loaded} />;
}

function mount(dep = pdDeployment()) {
  render(<I18nextProvider i18n={i18n}><Schedules dep={dep} /></I18nextProvider>);
  return userEvent.setup();
}

const section = () => screen.getByRole("region", { name: a.schedule.title });
const dialog = () => within(screen.getByRole("dialog"));
const roleName = (role: string) => i18n.t(`deployments:detail.pd.role.${role}`);
const orderText = (role: string, count: number) => i18n.t("deployments:automation.schedule.actionText.roleReplicas", { role: roleName(role), count });

async function openNewReplicasOrder(user: ReturnType<typeof userEvent.setup>) {
  await within(section()).findByText(a.schedule.empty);
  await user.click(within(section()).getByRole("button", { name: a.schedule.add }));
  return dialog();
}

async function pickRole(modal: ReturnType<typeof dialog>, role: string) {
  fireEvent.mouseDown(modal.getByRole("combobox", { name: a.schedule.field.role }));
  let option: Element | undefined;
  await waitFor(() => {
    option = [...document.querySelectorAll(".ant-select-item-option")].find((el) => el.textContent === roleName(role));
    if (!option) throw new Error(`option ${role} not offered`);
  });
  fireEvent.click(option!);
}

afterEach(() => { cleanup(); Object.assign(api, original); });
after(() => closeChannels());

test("a PD deployment placed automatically offers a replica count, and asks for the role", async () => {
  stub();
  const user = mount();
  const modal = await openNewReplicasOrder(user);
  assert.ok(modal.getByText(a.schedule.actionName.replicas));
  // The first role is chosen and the count starts from what that role runs now.
  assert.ok(modal.getByRole("combobox", { name: a.schedule.field.role }));
  assert.equal((modal.getByLabelText(a.schedule.field.replicas) as HTMLInputElement).value, "2");
  fireEvent.change(modal.getByLabelText(a.schedule.field.time), { target: { value: "09:30" } });
  fireEvent.change(modal.getByLabelText(a.schedule.field.replicas), { target: { value: "4" } });
  await user.click(modal.getByRole("button", { name: a.schedule.save }));
  await waitFor(() => assert.deepEqual(created, [{ action: "replicas", weekdays: [1, 2, 3, 4, 5], time: "09:30", timezone: "", replicas: 4, role: "decode", enabled: true }]));
  await within(section()).findByText(orderText("decode", 4));
});

test("the role picked decides the fewest replicas: prefill may be scheduled to 0, decode may not", async () => {
  stub();
  const user = mount();
  const modal = await openNewReplicasOrder(user);
  fireEvent.change(modal.getByLabelText(a.schedule.field.time), { target: { value: "01:00" } });
  // Decode (the first role) is never empty: the count box stops at 1.
  const count = modal.getByLabelText(a.schedule.field.replicas);
  assert.equal(count.getAttribute("aria-valuemin"), "1");
  assert.equal(count.getAttribute("aria-valuemax"), "32");

  await pickRole(modal, "prefill");
  await waitFor(() => assert.equal(modal.getByLabelText(a.schedule.field.replicas).getAttribute("aria-valuemin"), "0"));
  fireEvent.change(modal.getByLabelText(a.schedule.field.replicas), { target: { value: "0" } });
  await user.click(modal.getByRole("button", { name: a.schedule.save }));
  await waitFor(() => assert.deepEqual(created, [{ action: "replicas", weekdays: [1, 2, 3, 4, 5], time: "01:00", timezone: "", replicas: 0, role: "prefill", enabled: true }]));
  await within(section()).findByText(orderText("prefill", 0));
});

test("an order of a deployment that is not PD names no role", async () => {
  stub();
  const plain = pdDeployment({ pd: undefined, placement_mode: undefined });
  const user = mount(plain);
  const modal = await openNewReplicasOrder(user);
  assert.equal(modal.queryByRole("combobox", { name: a.schedule.field.role }), null);
  fireEvent.change(modal.getByLabelText(a.schedule.field.time), { target: { value: "09:30" } });
  await user.click(modal.getByRole("button", { name: a.schedule.save }));
  await waitFor(() => assert.equal(created.length, 1));
  assert.equal("role" in created[0], false);
});

test("a PD deployment placed by hand cannot be given a replica count", async () => {
  stub();
  mount(pdDeployment({ placement_mode: "manual" }));
  await within(section()).findByText(a.schedule.noActions);
  assert.equal((within(section()).getByRole("button", { name: a.schedule.add }) as HTMLButtonElement).disabled, true);
});

test("editing an order keeps its role, and the switch restates it", async () => {
  stub();
  schedules = [{ id: 8, action: "replicas", weekdays: [1], time: "07:00", timezone: "", replicas: 3, role: "decode", enabled: true, next_at: "2026-10-30T07:00:00Z" }];
  const user = mount();
  await within(section()).findByText(orderText("decode", 3));
  await user.click(within(section()).getByRole("switch", { name: i18n.t("deployments:automation.schedule.enabledOf", { order: orderText("decode", 3), time: "07:00" }) }));
  await waitFor(() => assert.deepEqual(updated, [[8, { action: "replicas", weekdays: [1], time: "07:00", timezone: "", replicas: 3, role: "decode", enabled: false }]]));
  await user.click(within(section()).getByRole("button", { name: a.schedule.edit }));
  const modal = dialog();
  assert.equal((modal.getByLabelText(a.schedule.field.replicas) as HTMLInputElement).value, "3");
  fireEvent.change(modal.getByLabelText(a.schedule.field.replicas), { target: { value: "5" } });
  await user.click(modal.getByRole("button", { name: a.schedule.save }));
  await waitFor(() => assert.equal(updated.at(-1)?.[1].role, "decode"));
  assert.equal(updated.at(-1)?.[1].replicas, 5);
});

test("the server's refusal of a role is worded by the role field's label", async () => {
  stub();
  const user = mount();
  const modal = await openNewReplicasOrder(user);
  fireEvent.change(modal.getByLabelText(a.schedule.field.time), { target: { value: "09:30" } });
  api.createSchedule = async () => {
    throw new APIError("SCHEDULE_INVALID", { field: "role", reason: 'the vllm-nixl mode has no "encode" role' }, "schedule role: the vllm-nixl mode has no \"encode\" role", 400);
  };
  await user.click(modal.getByRole("button", { name: a.schedule.save }));
  await modal.findByText(i18n.t("common:apiError.SCHEDULE_INVALID", { field: a.schedule.field.role, reason: 'the vllm-nixl mode has no "encode" role' }));
});

test("the scheduling policy summary names the role of the next order", async () => {
  stub();
  schedules = [{ id: 8, action: "replicas", weekdays: [1], time: "07:00", timezone: "", replicas: 0, role: "prefill", enabled: true, next_at: "2026-10-30T07:00:00Z" }];
  api.deploymentSchedule = async () => ({ items: schedules, total: schedules.length });
  const dep = pdDeployment();
  render(<I18nextProvider i18n={i18n}><DeploymentSchedulingPolicy dep={dep} onChanged={() => {}} /></I18nextProvider>);
  const group = await screen.findByRole("region", { name: locale.deployments.schedulingPolicy.title });
  await waitFor(() => assert.ok(group.textContent?.includes(orderText("prefill", 0)), group.textContent ?? ""));
});

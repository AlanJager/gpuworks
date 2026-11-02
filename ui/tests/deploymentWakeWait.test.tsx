import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import * as shapes from "./helpers/deploymentCapabilities.ts";

// A wake the cards cannot take: the refusal shows with its two ways on
// (wait for room, or put the occupants to sleep and wake), a card held by a
// process outside the platform offers neither, and a wake that waits shows a
// banner that can be cancelled.

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["deployments"]);
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { formatTime } = await import("../src/i18n/datetime.ts");
const { default: SleepControls } = await import("../src/pages/DeploymentSleepControls.tsx");
type Deployment = import("../src/api.ts").Deployment;
type WakeOptions = import("../src/api.ts").WakeOptions;

const sl = locale.deployments.sleep;
const original = { ...api };

function instance(uuid: string, index: number) {
  return {
    uuid, deployment_id: 1, node_uuid: "n1", state: "running", gpu_indexes: [0], ports: [30000], api_port: 30000,
    image: "runner", message: "", sleep_level: 1, desired_sleep: 1, asleep: true, replica: { index },
    can_sleep_alone: true, can_wake_alone: true, dist_role: "leader", nnodes: 1, node_rank: 0,
  };
}

function asleep(overrides: Record<string, unknown> = {}): Deployment {
  return {
    id: 1, name: "qwen", model_id: 1, model_name: "m", served_name: "m", engine: "vllm", node_uuid: "n1",
    desired_state: "active", runtime_state: "running", sleepable: true, replicas: 1, max_replicas: 32,
    health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] },
    template_deviated: false, tokenize_enabled: false, created_at: "2026-10-01T00:00:00Z",
    instances: [instance("inst-a", 0)],
    ...shapes.sleepableSingleNode,
    ...overrides,
  } as unknown as Deployment;
}

const shortage = () => new APIError(
  "WAKE_INSUFFICIENT_GPU_MEMORY",
  { node: "n1", gpu_indexes: [1], shortfall_gib: "21.6", occupants: ["other", "third"] },
  "GPU [1] cannot take it back",
  409,
);
const shortageText = () => i18n.t("common:apiError.WAKE_INSUFFICIENT_GPU_MEMORY", { node: "n1", gpu_indexes: [1], shortfall_gib: "21.6", occupants: ["other", "third"] });

let wakes: WakeOptions[] = [];

function stub(wake: (opts: WakeOptions) => Promise<unknown>) {
  wakes = [];
  api.deploymentEvents = (async () => ({ items: [] })) as typeof api.deploymentEvents;
  api.wakeDeployment = (async (_id: number, opts: WakeOptions = {}) => { wakes.push(opts); return wake(opts); }) as typeof api.wakeDeployment;
}

function mount(dep = asleep(), onDone = () => {}) {
  render(<I18nextProvider i18n={i18n}><SleepControls dep={dep} onDone={onDone} /></I18nextProvider>);
  return userEvent.setup();
}

const button = (name: string) => screen.getByText(name).closest("button") as HTMLButtonElement;

afterEach(() => { cleanup(); Object.assign(api, original); });
after(() => closeChannels());

test("a wake the cards cannot take shows the refusal with a way to wait and a way to yield, and no error toast", async () => {
  stub(async () => { throw shortage(); });
  const user = mount();
  await user.click(screen.getByRole("button", { name: sl.wake }));
  const alert = await screen.findByRole("alert");
  assert.ok(within(alert).getByText(sl.refused.title));
  assert.ok(within(alert).getByText(shortageText()));
  assert.ok(within(alert).getByText(sl.refused.wait));
  assert.ok(within(alert).getByText(i18n.t("deployments:sleep.refused.yield", { occupants: "other, third" })));
  // How long waiting lasts is the server's limit (wake_wait_limit_sec 1800).
  assert.ok(within(alert).getByText(i18n.t("deployments:sleep.refused.hint", { limit: "30 min" })));
  // The refusal is shown in place, not twice.
  assert.equal(screen.queryByText(i18n.t("deployments:sleep.wakeFailed", { detail: shortageText() })), null);
});

test("Wait for room wakes with wait=true, says it waits and drops the refusal", async () => {
  stub(async (opts) => {
    if (opts.wait) return { status: "waiting", woken: [], waiting: ["inst-a"], yielded: [] };
    throw shortage();
  });
  let done = 0;
  const user = mount(asleep(), () => { done += 1; });
  await user.click(screen.getByRole("button", { name: sl.wake }));
  await screen.findByText(sl.refused.title);
  const before = done;
  await user.click(button(sl.refused.wait));
  await waitFor(() => assert.deepEqual(wakes, [{ partial: false }, { wait: true }]));
  await screen.findByText(sl.waitStarted);
  await waitFor(() => assert.equal(screen.queryByText(sl.refused.title), null));
  assert.equal(done, before + 1);
});

test("Put the occupants to sleep, then wake wakes with yield=true and names who was put to sleep", async () => {
  stub(async (opts) => {
    // The server names who it put to sleep: here only one of the two the
    // refusal named, and the result says that one.
    if (opts.yield) return { status: "waiting", woken: [], waiting: ["inst-a"], yielded: ["other"] };
    throw shortage();
  });
  const user = mount();
  await user.click(screen.getByRole("button", { name: sl.wake }));
  await screen.findByText(sl.refused.title);
  await confirmYield(user, "other, third");
  await waitFor(() => assert.deepEqual(wakes, [{ partial: false }, { yield: true }]));
  await screen.findByText(i18n.t("deployments:sleep.yieldStarted", { names: "other" }));
  await waitFor(() => assert.equal(screen.queryByText(sl.refused.title), null));
});

test("putting the occupants to sleep is confirmed first, saying they stay asleep until woken by hand; cancelling changes nothing", async () => {
  stub(async () => { throw shortage(); });
  const user = mount();
  await user.click(screen.getByRole("button", { name: sl.wake }));
  await screen.findByText(sl.refused.title);
  await user.click(button(i18n.t("deployments:sleep.refused.yield", { occupants: "other, third" })));
  const confirm = (await screen.findByText(i18n.t("deployments:sleep.refused.yieldConfirm", { occupants: "other, third" }))).closest(".ant-popover") as HTMLElement;
  assert.ok(within(confirm).getByText(i18n.t("deployments:sleep.refused.yieldHeld", { occupants: "other, third" })));
  // Nothing is ordered until the operator confirms.
  assert.deepEqual(wakes, [{ partial: false }]);
  await user.click(within(confirm).getByRole("button", { name: sl.refused.yieldCancel }));
  assert.deepEqual(wakes, [{ partial: false }]);
  assert.ok(screen.getByText(sl.refused.title));
});

// confirmYield clicks the yield button for occupants and confirms it in its
// popover.
async function confirmYield(user: ReturnType<typeof mount>, occupants: string) {
  await user.click(button(i18n.t("deployments:sleep.refused.yield", { occupants })));
  const confirm = (await screen.findByText(i18n.t("deployments:sleep.refused.yieldConfirm", { occupants }))).closest(".ant-popover") as HTMLElement;
  await user.click(within(confirm).getByRole("button", { name: sl.refused.yieldOk }));
}

test("a yield an occupant blocks is refused with the reason and the choice stays", async () => {
  stub(async (opts) => {
    if (opts.yield) throw new APIError("WAKE_YIELD_BLOCKED", { occupants: ["other"], woken: [] }, "cannot make room", 409);
    throw shortage();
  });
  const user = mount();
  await user.click(screen.getByRole("button", { name: sl.wake }));
  await screen.findByText(sl.refused.title);
  await confirmYield(user, "other, third");
  await screen.findByText(i18n.t("deployments:sleep.wakeFailed", { detail: i18n.t("common:apiError.WAKE_YIELD_BLOCKED", { occupants: ["other"], woken: [] }) }));
  assert.ok(screen.getByText(sl.refused.title));
});

test("a card held outside the platform shows its message only: no waiting, no yielding", async () => {
  stub(async () => {
    throw new APIError("WAKE_GPU_HELD_OUTSIDE", { node: "n1", gpu_indexes: [1], shortfall_gib: "21.6", occupants: [] }, "held outside", 409);
  });
  const user = mount();
  await user.click(screen.getByRole("button", { name: sl.wake }));
  const alert = await screen.findByRole("alert");
  assert.ok(within(alert).getByText(sl.refused.heldOutside));
  assert.ok(within(alert).getByText(i18n.t("common:apiError.WAKE_GPU_HELD_OUTSIDE", { node: "n1", gpu_indexes: [1], shortfall_gib: "21.6" })));
  assert.equal(screen.queryByText(sl.refused.wait), null);
  assert.equal(screen.queryByText(/to sleep, then wake/), null);
});

test("how long a wake waits is the server's limit, in words", async () => {
  stub(async () => ({ status: "waking" }));
  mount(asleep({ wake_waiting_since: "2026-10-25T09:00:00Z", wake_wait_limit_sec: 7200 }));
  const banner = screen.getByText(i18n.t("deployments:sleep.waiting.since", { time: formatTime("2026-10-25T09:00:00Z") })).closest(".ant-alert") as HTMLElement;
  assert.ok(within(banner).getByText(i18n.t("deployments:sleep.waiting.hint", { limit: "2 h" })));
  assert.equal(within(banner).queryByText(/30 min/), null);
});

test("a wake that is waiting shows since when and is cancelled from the keyboard", async () => {
  stub(async () => ({ status: "waking" }));
  const ended: number[] = [];
  api.endWakeWaiting = async (id) => { ended.push(id); return { items: ["inst-a"], total: 1 }; };
  let done = 0;
  const user = mount(asleep({ wake_waiting_since: "2026-10-25T09:00:00Z" }), () => { done += 1; });
  const banner = screen.getByText(i18n.t("deployments:sleep.waiting.since", { time: formatTime("2026-10-25T09:00:00Z") })).closest(".ant-alert") as HTMLElement;
  assert.ok(within(banner).getByText(i18n.t("deployments:sleep.waiting.hint", { limit: "30 min" })));
  const cancel = within(banner).getByRole("button", { name: sl.waiting.cancel });
  cancel.focus();
  assert.equal(document.activeElement, cancel);
  await user.keyboard("{Enter}");
  await waitFor(() => assert.deepEqual(ended, [1]));
  await screen.findByText(sl.waiting.cancelled);
  assert.equal(done, 1);
});

test("a cancel the server refuses keeps the banner and says why", async () => {
  stub(async () => ({ status: "waking" }));
  api.endWakeWaiting = async () => { throw new APIError("DEPLOYMENT_DELETING", {}, "being deleted", 409); };
  const user = mount(asleep({ wake_waiting_since: "2026-10-25T09:00:00Z" }));
  await user.click(screen.getByRole("button", { name: sl.waiting.cancel }));
  await screen.findByText(i18n.t("deployments:sleep.waiting.cancelFailed", { detail: i18n.t("common:apiError.DEPLOYMENT_DELETING") }));
  assert.ok(screen.getByText(i18n.t("deployments:sleep.waiting.since", { time: formatTime("2026-10-25T09:00:00Z") })));
});

test("without a waiting wake there is no banner", async () => {
  stub(async () => ({ status: "waking" }));
  mount();
  assert.equal(screen.queryByText(sl.waiting.cancel), null);
});

test("an asleep deployment says a request wakes it, unless the sleep was ordered by an operator or a schedule", async () => {
  stub(async () => ({ status: "woken", woken: ["inst-a"], waiting: [], yielded: [] }));
  mount(asleep({ sleep_hold: "none" }));
  screen.getByText(sl.requestWakesHint);
  assert.equal(screen.queryByText(sl.heldHint), null);
  cleanup();
  mount(asleep({ sleep_hold: "all", sleep_held: true }));
  screen.getByText(sl.heldHint);
  assert.equal(screen.queryByText(sl.requestWakesHint), null);
  cleanup();
  // The server reads some replicas held and the others not: the hint says a
  // request wakes only the others, and says neither of the plain hints.
  mount(asleep({
    sleep_hold: "some", sleep_held: false, replicas: 2,
    instances: [{ ...instance("inst-a", 0), sleep_held: true }, { ...instance("inst-b", 1), sleep_held: false }],
  }));
  screen.getByText(sl.someHeldHint);
  assert.equal(screen.queryByText(sl.heldHint), null);
  assert.equal(screen.queryByText(sl.requestWakesHint), null);
  cleanup();
  // An awake deployment has no sleep to explain.
  mount(asleep({ instances: [{ ...instance("inst-a", 0), sleep_level: 0, desired_sleep: 0, asleep: false }] }));
  assert.equal(screen.queryByText(sl.requestWakesHint), null);
  assert.equal(screen.queryByText(sl.heldHint), null);
});

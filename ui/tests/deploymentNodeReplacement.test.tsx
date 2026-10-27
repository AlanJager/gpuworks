import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// A deployment's node replacement setting: the wait before a replica leaves a
// node that stays offline (default 10 minutes, 0 never moves it) and what the
// server refuses.

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["deployments", "deploy"]);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: DeploymentNodeReplacement } = await import("../src/pages/DeploymentNodeReplacement.tsx");
type Deployment = import("../src/api.ts").Deployment;

const n = locale.deployments.nodeReplacement;
const original = { ...api };

function deployment(overrides: Record<string, unknown> = {}): Deployment {
  return {
    id: 1, name: "qwen", model_id: 1, model_name: "m", served_name: "m", engine: "vllm", node_uuid: "n1",
    desired_state: "active", runtime_state: "running", sleepable: true, replicas: 2, max_replicas: 32, nnodes: 1,
    health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] },
    template_deviated: false, tokenize_enabled: false, created_at: "2026-10-01T00:00:00Z",
    node_replacement: { after_sec: 600 },
    ...overrides,
  } as unknown as Deployment;
}

function mount(dep = deployment(), onChanged = () => {}) {
  render(<I18nextProvider i18n={i18n}><DeploymentNodeReplacement dep={dep} onChanged={onChanged} /></I18nextProvider>);
  return userEvent.setup();
}

// A button found by its label: after an order the loading icon's leave motion
// never ends in jsdom and stays in the button's accessible name.
const labelled = (scope: ReturnType<typeof within>, text: string) => scope.getByText(text).closest("button") as HTMLButtonElement;

afterEach(() => { cleanup(); Object.assign(api, original); });
after(() => closeChannels());

test("the wait before a replica leaves a lost node starts at 10 minutes, explains itself and saves seconds", async () => {
  const saved: unknown[] = [];
  let changed = 0;
  api.setNodeReplacement = async (_id, body) => { saved.push(body); return body; };
  const user = mount(deployment({ node_replacement: undefined }), () => { changed++; });
  const section = screen.getByRole("region", { name: n.title });
  const minutes = within(section).getByLabelText(n.minutes) as HTMLInputElement;
  assert.equal(minutes.value, "10");
  assert.ok(within(section).getByText(new RegExp(n.hintSingle)));
  assert.ok(within(section).getByText(new RegExp(n.hintRevert.slice(0, 40))));
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

test("a saved setting is shown as saved, 0 reads as never, and saving needs a change", () => {
  mount(deployment({ node_replacement: { after_sec: 0 } }));
  const section = screen.getByRole("region", { name: n.title });
  assert.equal((within(section).getByLabelText(n.minutes) as HTMLInputElement).value, "0");
  assert.equal((within(section).getByRole("button", { name: n.save }) as HTMLButtonElement).disabled, true);
  cleanup();
  mount(deployment({ node_replacement: { after_sec: 5400 } }));
  assert.equal((within(screen.getByRole("region", { name: n.title })).getByLabelText(n.minutes) as HTMLInputElement).value, "90");
});

test("a multi-node or PD deployment's explanation names the compute network and architecture; a refusal is shown", async () => {
  api.setNodeReplacement = async () => { throw new APIError("NODE_REPLACEMENT_INVALID", { minimum: 60, maximum: 604800 }, "out of range", 400); };
  const user = mount(deployment({ nnodes: 2 }));
  const section = screen.getByRole("region", { name: n.title });
  assert.ok(within(section).getByText(new RegExp(n.hintMulti.slice(0, 40))));
  fireEvent.change(within(section).getByLabelText(n.minutes), { target: { value: "1.5" } });
  fireEvent.change(within(section).getByLabelText(n.minutes), { target: { value: "5" } });
  await user.click(within(section).getByRole("button", { name: n.save }));
  await screen.findByText(i18n.t("deployments:nodeReplacement.saveFailed", { detail: i18n.t("common:apiError.NODE_REPLACEMENT_INVALID", { minimum: 60, maximum: 604800 }) }));
  cleanup();
  mount(deployment({ pd: { mode: "pd_disagg", roles: [] } }));
  assert.ok(within(screen.getByRole("region", { name: n.title })).getByText(new RegExp(n.hintMulti.slice(0, 40))));
});

test("a deployment being deleted shows no node replacement setting", () => {
  mount(deployment({ desired_state: "deleting" }));
  assert.equal(screen.queryByRole("region", { name: n.title }), null);
});

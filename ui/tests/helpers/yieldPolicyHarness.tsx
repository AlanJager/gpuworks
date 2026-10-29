import assert from "node:assert/strict";
import { after, afterEach } from "node:test";
import React, { useState } from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./dom.ts";
import { trackMessageChannels } from "./trackedMessageChannels.ts";

// The harness of the yield policy editor's tests (deploymentYieldPolicy and
// deploymentYieldPolicySave): the real English locale, a scheduling policy
// card on a server stub that answers as the wire contract says, and finders
// for the editor's controls.

const closeChannels = trackMessageChannels();
export const { i18n, locale } = await createI18n(["deployments", "deploy"]);
// user-event walks a radio group by CSS.escape, which jsdom does not have.
(window as unknown as { CSS?: unknown }).CSS ??= { escape: (value: string) => value.replace(/[^\w-]/g, (char) => `\\${char}`) };
export const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
export const userEvent = (await import("@testing-library/user-event")).default;
export const { api } = await import("../../src/api.ts");
export const { APIError } = await import("../../src/apiError.ts");
const { default: DeploymentSchedulingPolicy } = await import("../../src/pages/DeploymentSchedulingPolicy.tsx");
const { draftOfParams, paramsOfDraft, presetOfDraft } = await import("../../src/pages/yieldPolicy/model.ts");
export type Deployment = import("../../src/api.ts").Deployment;
export type YieldPolicy = import("../../src/api.ts").YieldPolicy;
export type YieldPolicyRequest = import("../../src/api.ts").YieldPolicyRequest;

export const p = locale.deployments.schedulingPolicy;
export const y = locale.deployments.yieldPolicy;
const original = { ...api };

export function deployment(overrides: Record<string, unknown> = {}): Deployment {
  return {
    id: 1, name: "qwen", model_id: 1, model_name: "m", served_name: "m", engine: "vllm", node_uuid: "n1",
    desired_state: "active", runtime_state: "running", sleepable: true, replicas: 1, max_replicas: 32, nnodes: 1,
    health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] },
    template_deviated: false, tokenize_enabled: false, created_at: "2026-10-01T00:00:00Z",
    node_replacement: { after_sec: 600 },
    ...overrides,
  } as unknown as Deployment;
}

// held is a policy as the server reports it: normalized, with its derived preset.
export const held = (policy: Omit<YieldPolicy, "preset">): YieldPolicy => ({ preset: presetOfDraft(draftOfParams(policy)), ...paramsOfDraft(draftOfParams(policy)) });

// What the server holds: the stub writes here and the host reloads from it, as
// the deployment page does after onChanged.
export const state = {
  current: undefined as unknown as Deployment,
  role: "admin",
  changed: 0,
  sent: [] as Array<[number, YieldPolicyRequest]>,
};

// The server's own checks, as faithfully as the wire contract states them: a
// full object needs a policy, the numeric fields of the policy are held to
// their ranges, and what is stored is the normalized policy with its preset.
export function serverAnswer(body: YieldPolicyRequest): YieldPolicy {
  if ("preset" in body) return held(paramsOfDraft(draftOfParams(presets[body.preset])));
  const ranges: Record<string, [number, number]> = { idle_sec: [30, 604800], min_serve_sec: [0, 3600], unfinished_wait_sec: [1, 600], hold_max_sec: [10, 1800] };
  const draft = draftOfParams(body as never);
  for (const [field, [minimum, maximum]] of Object.entries(ranges)) {
    const value = (body as Record<string, unknown>)[field];
    if (typeof value === "number" && (value < minimum || value > maximum)) {
      throw new APIError("YIELD_POLICY_INVALID", { field, reason: "out_of_range", minimum, maximum }, "out of range", 400);
    }
  }
  return held(paramsOfDraft(draft));
}
export const presets = {
  resident: { policy: "never", auto_sleep: false },
  online: { policy: "idle", auto_sleep: false, idle_sec: 300, sleep_level: "auto", hold_max_sec: 120 },
  shared: { policy: "timeslice", auto_sleep: false, idle_sec: 60, sleep_level: "auto", min_serve_sec: 120, unfinished: "wait", unfinished_wait_sec: 30, hold_max_sec: 600 },
  offline: { policy: "anytime", auto_sleep: false, sleep_level: "auto", unfinished: "abort", hold_max_sec: 1800 },
} as const;

export function stub() {
  state.role = "admin";
  state.changed = 0;
  state.sent.length = 0;
  api.me = async () => ({ id: 1, username: "u", role: state.role }) as Awaited<ReturnType<typeof api.me>>;
  api.deploymentSchedule = async () => ({ items: [], total: 0 });
  api.setYieldPolicy = async (id, body) => {
    state.sent.push([id, body]);
    const answer = serverAnswer(body);
    state.current = { ...state.current, yield_policy: answer };
    return answer;
  };
}

function Host() {
  const [dep, setDep] = useState(state.current);
  return <DeploymentSchedulingPolicy dep={dep} onChanged={() => { state.changed += 1; setDep(state.current); }} />;
}

export function mount(dep = deployment()) {
  state.current = dep;
  render(<I18nextProvider i18n={i18n}><Host /></I18nextProvider>);
  return userEvent.setup();
}

export const withPolicy = (policy: Omit<YieldPolicy, "preset">) => deployment({ yield_policy: held(policy) });
export const summaryOf = (value: string) => i18n.t("deployments:schedulingPolicy.item", { label: y.summaryLabel, value });
export const summaryText = async () => (await screen.findByRole("region", { name: p.title })).textContent ?? "";

export async function openEditor(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole("button", { name: p.edit }));
  await user.click(await screen.findByRole("tab", { name: y.title }));
  return within(await screen.findByRole("region", { name: y.title, hidden: false }));
}

export type Editor = Awaited<ReturnType<typeof openEditor>>;
export const radio = (editor: Editor, name: string) => editor.getByRole("radio", { name }) as HTMLInputElement;
export const spin = (editor: Editor, name: string) => editor.queryByRole("spinbutton", { name }) as HTMLInputElement | null;
export const saveButton = (editor: Editor) => editor.getByText(locale.deployments.automation.save).closest("button") as HTMLButtonElement;
export const f = y.field;

// Which parameter controls the editor shows right now.
export function controls(editor: Editor) {
  return {
    idle: spin(editor, f.idle_sec) !== null,
    idleDash: within(editor.getByRole("group", { name: f.idle_sec })).queryByText(y.none) !== null,
    autoSleep: editor.queryByRole("switch", { name: f.auto_sleep }) !== null,
    level: editor.queryByRole("combobox", { name: f.sleep_level }) !== null,
    minServe: spin(editor, f.min_serve_sec) !== null,
    unfinished: editor.queryByRole("radiogroup", { name: f.unfinished }) !== null,
    wait: spin(editor, f.unfinished_wait_sec) !== null,
    hold: spin(editor, f.hold_max_sec) !== null,
  };
}

export async function choosePolicy(user: ReturnType<typeof userEvent.setup>, editor: Editor, policy: keyof typeof y.policy.name) {
  await user.click(editor.getByRole("combobox", { name: y.policyLabel }));
  await user.click(await screen.findByText(`${y.policy.name[policy]} - ${y.policy.description[policy]}`));
}

afterEach(() => { cleanup(); Object.assign(api, original); });
after(() => closeChannels());

import assert from "node:assert/strict";
import { after, afterEach } from "node:test";
import fs from "node:fs";
import React, { useState } from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./dom.ts";
import { trackMessageChannels } from "./trackedMessageChannels.ts";
import * as shapes from "./deploymentCapabilities.ts";

// The harness of the yield policy editor's tests (deploymentYieldPolicy and
// deploymentYieldPolicySave): the real English locale, a scheduling policy
// card on a server stub that answers as the wire contract says, and finders
// for the editor's controls.

const closeChannels = trackMessageChannels();
export const { i18n, locale } = await createI18n(["deployments", "deploy"]);
// user-event walks a radio group by CSS.escape, which jsdom does not have.
(window as unknown as { CSS?: unknown }).CSS ??= { escape: (value: string) => value.replace(/[^\w-]/g, (char) => `\\${char}`) };
export const { message } = await import("antd");
export const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
export const userEvent = (await import("@testing-library/user-event")).default;
export const { api } = await import("../../src/api.ts");
export const { APIError } = await import("../../src/apiError.ts");
const { default: DeploymentSchedulingPolicy } = await import("../../src/pages/DeploymentSchedulingPolicy.tsx");
export type Deployment = import("../../src/api.ts").Deployment;
export type YieldPolicy = import("../../src/api.ts").YieldPolicy;
export type YieldPolicyRequest = import("../../src/api.ts").YieldPolicyRequest;
export type YieldPolicyAnswer = import("../../src/api.ts").YieldPolicyAnswer;
export type YieldPolicyDictionary = import("../../src/api.ts").YieldPolicyDictionary;
type YieldParamName = import("../../src/api.ts").YieldParamName;

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
    ...shapes.sleepableSingleNode,
    ...overrides,
  } as unknown as Deployment;
}

// The dictionary as the server answers GET /scheduling/yield-policies, literally.
export const dictionary = JSON.parse(fs.readFileSync(new URL("../fixtures/yield-policies.json", import.meta.url), "utf8")) as YieldPolicyDictionary;
// The default parameters of each policy, as the dictionary states them.
export const defaults = Object.fromEntries(dictionary.policies.map((entry) => [entry.policy, entry.defaults])) as Record<"never" | "idle" | "timeslice" | "anytime", YieldPolicy>;

// What the server holds: the stub writes here and the host reloads from it, as
// the deployment page does after onChanged.
export const state = {
  current: undefined as unknown as Deployment,
  role: "admin",
  changed: 0,
  sent: [] as Array<[number, YieldPolicyRequest]>,
  dictionaryReads: 0,
  dictionaryMode: "ok" as "ok" | "error" | "pending",
  // The other idle-policy deployments the next PUT answer names.
  neighbours: [] as string[],
};

const refuse = (field: string, reason: string, extra: Record<string, unknown> = {}) =>
  new APIError("YIELD_POLICY_INVALID", { field, reason, ...extra }, `${field}: ${reason}`, 400);

// The server's own checks, as faithfully as the wire contract states them: a
// body names a known policy and nothing unknown (the old preset is an unknown
// field), a parameter missing takes the policy's default, one that does not
// apply is left out of what is stored, numbers are held to the dictionary's
// bounds, and idle_sec may not exceed max_serve_sec when both apply.
export function serverAnswer(body: YieldPolicyRequest): YieldPolicyAnswer {
  const known = new Set(["policy", "idle_sec", "sleep_level", "max_serve_sec", "unfinished", "unfinished_wait_sec", "hold_max_sec"]);
  for (const key of Object.keys(body)) if (!known.has(key)) throw new APIError("INVALID_REQUEST", { field: key }, `json: unknown field "${key}"`, 400);
  const entry = dictionary.policies.find((item) => item.policy === body.policy);
  if (!entry) throw refuse("policy", "invalid_value");
  const given = body as unknown as Record<string, unknown>;
  const stored: Record<string, unknown> = { policy: entry.policy };
  const wanted = new Set(entry.applies);
  for (const [name, when] of Object.entries(dictionary.applies_when)) {
    for (const [selector, values] of Object.entries(when ?? {})) {
      const held = given[selector] ?? (entry.defaults as Record<string, unknown>)[selector];
      if (!(values ?? []).includes(held as string)) wanted.delete(name as YieldParamName);
    }
  }
  for (const name of entry.applies) {
    if (!wanted.has(name)) continue;
    const value = given[name] ?? (entry.defaults as Record<string, unknown>)[name];
    if (name === "sleep_level" && !dictionary.sleep_levels.includes(value as never)) throw refuse(name, "invalid_value");
    if (name === "unfinished" && !dictionary.unfinished.includes(value as never)) throw refuse(name, "invalid_value");
    const bound = dictionary.bounds[name];
    if (bound) {
      if (typeof value !== "number") throw refuse(name, "required");
      if (value < bound.minimum || value > bound.maximum) throw refuse(name, "out_of_range", { minimum: bound.minimum, maximum: bound.maximum });
    }
    stored[name] = value;
  }
  for (const name of entry.applies) {
    const cap = dictionary.bounds[name]?.at_most;
    if (cap && wanted.has(cap) && wanted.has(name) && (stored[name] as number) > (stored[cap] as number)) {
      throw refuse(name, "out_of_range", { minimum: dictionary.bounds[name]?.minimum, maximum: stored[cap] });
    }
  }
  return { ...(stored as unknown as YieldPolicy), ...(state.neighbours.length > 0 ? { idle_neighbours: state.neighbours } : {}) };
}

// stubDictionary makes GET /scheduling/yield-policies answer, fail or stay
// pending.
export function stubDictionary(mode: "ok" | "error" | "pending" = "ok") {
  state.dictionaryMode = mode;
  api.yieldPolicies = async () => {
    state.dictionaryReads += 1;
    if (state.dictionaryMode === "error") throw new APIError("INTERNAL", {}, "dictionary unavailable", 500);
    if (state.dictionaryMode === "pending") return new Promise<YieldPolicyDictionary>(() => {});
    return structuredClone(dictionary);
  };
}

export function stub() {
  state.role = "admin";
  state.changed = 0;
  state.sent.length = 0;
  state.dictionaryReads = 0;
  state.neighbours = [];
  api.me = async () => ({ id: 1, username: "u", role: state.role }) as Awaited<ReturnType<typeof api.me>>;
  api.deploymentSchedule = async () => ({ items: [], total: 0 });
  stubDictionary();
  api.setYieldPolicy = async (id, body) => {
    state.sent.push([id, body]);
    const answer = serverAnswer(body);
    const { idle_neighbours: _neighbours, ...stored } = answer;
    state.current = { ...state.current, yield_policy: stored };
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

export const withPolicy = (policy: YieldPolicy) => deployment({ yield_policy: policy });
export const summaryOf = (value: string) => i18n.t("deployments:schedulingPolicy.item", { label: y.summaryLabel, value });
export const summaryText = async () => (await screen.findByRole("region", { name: p.title })).textContent ?? "";

export async function openEditor(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole("button", { name: p.edit }));
  await user.click(await screen.findByRole("tab", { name: y.title }));
  const editor = within(await screen.findByRole("region", { name: y.title, hidden: false }));
  await editor.findByRole("combobox", { name: y.policyLabel });
  return editor;
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
    maxServe: spin(editor, f.max_serve_sec) !== null,
    unfinished: editor.queryByRole("radiogroup", { name: f.unfinished }) !== null,
    wait: spin(editor, f.unfinished_wait_sec) !== null,
    level: editor.queryByRole("combobox", { name: f.sleep_level }) !== null,
    hold: spin(editor, f.hold_max_sec) !== null,
  };
}

export async function choosePolicy(user: ReturnType<typeof userEvent.setup>, editor: Editor, policy: keyof typeof y.policy.name) {
  await user.click(editor.getByRole("combobox", { name: y.policyLabel }));
  await user.click(await screen.findByText(`${y.policy.name[policy]} - ${y.policy.description[policy]}`));
}

// Toasts live outside the rendered tree: clear them so one test's does not answer another's query.
export const clearToasts = () => message.destroy();
afterEach(() => { cleanup(); clearToasts(); Object.assign(api, original); });
after(() => closeChannels());

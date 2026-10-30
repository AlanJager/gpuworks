import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React, { useState } from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// The service level part of a deployment's scheduling policy card: when the
// card shows it, what its summary says for each level, and the editor that
// sets it (success, the server's refusals, keyboard selection).

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["deployments", "deploy"]);
// user-event walks a radio group by CSS.escape, which jsdom does not have.
(window as unknown as { CSS?: unknown }).CSS ??= { escape: (value: string) => value.replace(/[^\w-]/g, (char) => `\\${char}`) };
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: DeploymentSchedulingPolicy } = await import("../src/pages/DeploymentSchedulingPolicy.tsx");
type Deployment = import("../src/api.ts").Deployment;

const p = locale.deployments.schedulingPolicy;
const s = locale.deployments.serviceLevel;
const original = { ...api };

function deployment(overrides: Record<string, unknown> = {}): Deployment {
  return {
    id: 1, name: "qwen", model_id: 1, model_name: "m", served_name: "m", engine: "vllm", node_uuid: "n1",
    desired_state: "active", runtime_state: "running", sleepable: true, replicas: 1, max_replicas: 32, nnodes: 1,
    health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] },
    template_deviated: false, tokenize_enabled: false, created_at: "2026-10-01T00:00:00Z",
    idle_sleep: { after_sec: 0, level: 2 }, node_replacement: { after_sec: 600 },
    ...overrides,
  } as unknown as Deployment;
}

// What the server holds: the stub writes here and the host reloads from it, as
// the deployment page does after onChanged.
let current: Deployment;
let role = "admin";
let changed = 0;
const sent: Array<[number, string]> = [];

function stub() {
  role = "admin";
  changed = 0;
  sent.length = 0;
  api.me = async () => ({ id: 1, username: "u", role }) as Awaited<ReturnType<typeof api.me>>;
  api.deploymentSchedule = async () => ({ items: [], total: 0 });
  api.setServiceLevel = async (id, level) => {
    sent.push([id, level]);
    current = { ...current, service_level: level };
    return current;
  };
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

const summaryOf = (value: string) => i18n.t("deployments:schedulingPolicy.item", { label: s.title, value });
const summaryText = async () => (await screen.findByRole("region", { name: p.title })).textContent ?? "";

async function openEditor(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole("button", { name: p.edit }));
  await user.click(await screen.findByRole("tab", { name: s.title }));
  return within(await screen.findByRole("region", { name: s.title, hidden: false }));
}

afterEach(() => { cleanup(); Object.assign(api, original); });
after(() => closeChannels());

test("the summary names each level, and a deployment with no level counts as resident", async () => {
  stub();
  for (const level of ["resident", "online", "shared", "offline"] as const) {
    mount(deployment({ service_level: level }));
    assert.ok((await summaryText()).includes(summaryOf(s.name[level])), level);
    cleanup();
  }
  mount(deployment());
  assert.ok((await summaryText()).includes(summaryOf(s.name.resident)));
});

test("the part shows for a sleepable single-node deployment, and for any deployment that has a level besides resident", async () => {
  stub();
  mount(deployment({ sleepable: false }));
  await screen.findByRole("region", { name: p.title });
  assert.ok(!(await summaryText()).includes(s.title));
  cleanup();
  // Multi-node and PD deployments cannot share GPUs by level.
  mount(deployment({ nnodes: 2 }));
  await screen.findByRole("region", { name: p.title });
  assert.ok(!(await summaryText()).includes(s.title));
  cleanup();
  mount(deployment({ pd: { mode: "ep", roles: [] } }));
  await screen.findByRole("region", { name: p.title });
  assert.ok(!(await summaryText()).includes(s.title));
  cleanup();
  // One that kept a level from when it could still shows it, to go back.
  mount(deployment({ sleepable: false, service_level: "offline" }));
  assert.ok((await summaryText()).includes(summaryOf(s.name.offline)));
  cleanup();
  mount(deployment());
  assert.ok((await summaryText()).includes(summaryOf(s.name.resident)));
});

test("the editor lists the four levels with their description and saves the chosen one", async () => {
  stub();
  const user = mount();
  const editor = await openEditor(user);
  const radios = editor.getAllByRole("radio");
  assert.equal(radios.length, 4);
  for (const level of ["resident", "online", "shared", "offline"] as const) {
    assert.ok(editor.getByText(s.description[level]));
  }
  assert.equal((editor.getByRole("radio", { name: new RegExp(s.name.resident) }) as HTMLInputElement).checked, true);
  const save = editor.getByText(locale.deployments.automation.save).closest("button") as HTMLButtonElement;
  assert.equal(save.disabled, true, "nothing changed yet");

  await user.click(editor.getByRole("radio", { name: new RegExp(s.name.shared) }));
  assert.equal(save.disabled, false);
  await user.click(save);
  await waitFor(() => assert.deepEqual(sent, [[1, "shared"]]));
  await waitFor(() => assert.equal(changed, 1));
  assert.ok((await screen.findAllByText(s.saved)).length > 0);
  await waitFor(() => assert.ok((screen.getByRole("region", { name: p.title }).textContent ?? "").includes(summaryOf(s.name.shared))));
});

test("a level can be chosen and saved from the keyboard", async () => {
  stub();
  const user = mount();
  const editor = await openEditor(user);
  editor.getByRole("radio", { name: new RegExp(s.name.resident) }).focus();
  await user.keyboard("{ArrowDown}");
  assert.equal((editor.getByRole("radio", { name: new RegExp(s.name.online) }) as HTMLInputElement).checked, true);
  const save = editor.getByText(locale.deployments.automation.save).closest("button") as HTMLButtonElement;
  save.focus();
  await user.keyboard("{Enter}");
  await waitFor(() => assert.deepEqual(sent, [[1, "online"]]));
  // Toasts of earlier cases may still be on the page.
  assert.ok((await screen.findAllByText(s.saved)).length > 0);
  await waitFor(() => assert.equal(changed, 1));
});

test("the server's refusal is shown in the user's language and nothing is marked saved", async () => {
  stub();
  api.setServiceLevel = async () => {
    throw new APIError("SERVICE_LEVEL_ONLINE_SHARED", { deployment: "other" }, "another online deployment shares its GPUs", 409);
  };
  const user = mount();
  const editor = await openEditor(user);
  await user.click(editor.getByRole("radio", { name: new RegExp(s.name.online) }));
  await user.click(editor.getByText(locale.deployments.automation.save).closest("button") as HTMLButtonElement);
  await screen.findByText(i18n.t("deployments:serviceLevel.saveFailed", { detail: i18n.t("common:apiError.SERVICE_LEVEL_ONLINE_SHARED", { deployment: "other" }) }));
  assert.equal(changed, 0);
  assert.deepEqual(sent, []);
});

test("a deployment that cannot share GPUs says so and offers only resident", async () => {
  stub();
  const user = mount(deployment({ sleepable: false, service_level: "shared" }));
  const editor = await openEditor(user);
  editor.getByText(s.notEligible);
  assert.equal((editor.getByRole("radio", { name: new RegExp(s.name.shared) }) as HTMLInputElement).checked, true);
  assert.equal((editor.getByRole("radio", { name: new RegExp(s.name.online) }) as HTMLInputElement).disabled, true);
  assert.equal((editor.getByRole("radio", { name: new RegExp(s.name.resident) }) as HTMLInputElement).disabled, false);
  await user.click(editor.getByRole("radio", { name: new RegExp(s.name.resident) }));
  await user.click(editor.getByText(locale.deployments.automation.save).closest("button") as HTMLButtonElement);
  await waitFor(() => assert.deepEqual(sent, [[1, "resident"]]));
});

test("a read-only user sees the level but cannot edit it", async () => {
  stub();
  role = "viewer";
  mount(deployment({ service_level: "online" }));
  assert.ok((await summaryText()).includes(summaryOf(s.name.online)));
  await waitFor(() => assert.equal(screen.queryByRole("button", { name: p.edit }), null));
});

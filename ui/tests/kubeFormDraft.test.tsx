import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { field, freshPopupFramesPerTest } from "./helpers/platformDialog.ts";
import { KUBE_DEFAULTS, kubeEntry, servePlatforms } from "./helpers/platformFixtures.ts";

const { i18n, locale } = await createI18n(["providers", "kubernetes"]);
const { act, cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { default: CloudProvidersPage } = await import("../src/pages/CloudProvidersPage.tsx");
const { FeedbackHost, message } = await import("../src/ui-kit/feedback/index.tsx");
type Update = import("../src/api.ts").KubeClusterUpdate;
const k = locale.kubernetes;
const originalApi = { ...api };
const originalInterval = globalThis.setInterval;
const originalHidden = Object.getOwnPropertyDescriptor(document, "hidden");
const polls = new Map<ReturnType<typeof setInterval>, () => void>();
await freshPopupFramesPerTest();

// Deliver only the page's actual 15-second polling callbacks on demand.
// Animation and input timers stay real; HTTP responses still pass through
// usePolling and reconstruct fresh entry objects, as JSON transport does.
function controlledPolling() {
  Object.defineProperty(document, "hidden", { configurable: true, value: false });
  globalThis.setInterval = ((callback: () => void, delay: number, ...args: unknown[]) => {
    if (delay !== 15000) return originalInterval(callback, delay, ...args);
    const timer = originalInterval(() => {}, delay);
    polls.set(timer, callback);
    return timer;
  }) as typeof setInterval;
}

afterEach(() => {
  message.destroy();
  cleanup();
  Object.assign(api, originalApi);
  globalThis.setInterval = originalInterval;
  if (originalHidden) Object.defineProperty(document, "hidden", originalHidden);
  else Reflect.deleteProperty(document, "hidden");
  for (const timer of polls.keys()) clearInterval(timer);
  polls.clear();
});

test("the real platform-page poll preserves an open Kubernetes edit and saves only that draft's changes", async () => {
  controlledPolling();
  const initial = kubeEntry({ runtime_class: "old-runtime", image_pull_secret: "old-secret", agent_image: "registry.example/agent:owned", agent_image_source: "quay" });
  const server = servePlatforms(api, { clusters: [initial] });
  api.kubeDefaults = async () => ({
    ...KUBE_DEFAULTS, agent_image_source: "swr", agent_image: "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-agent:r1",
    agent_image_sources: [
      { id: "swr", image: "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-agent:r1" },
      { id: "dockerhub", image: "gpuworks/gpuworks-agent:r1" },
      { id: "quay", image: "quay.io/gpuworks/gpuworks-agent:r1" },
    ],
  });
  const readClusters = api.kubeClusters;
  api.kubeClusters = async () => JSON.parse(JSON.stringify(await readClusters()));
  const updates: Array<{ uuid: string; body: Update }> = [];
  api.updateKubeCluster = async (uuid, body) => {
    updates.push({ uuid, body: structuredClone(body) });
    assert.ok(!(server.clusters instanceof Error));
    const saved = { ...server.clusters[0], ...body };
    server.clusters = [saved];
    return { cluster: saved, changed: Object.keys(body), reapply: false, commands: [] };
  };
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/providers"]}><FeedbackHost /><CloudProvidersPage /></MemoryRouter></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  const openEdit = async () => {
    const row = (await screen.findByText(initial.name, { selector: "td" })).closest("tr")!;
    await user.click(within(row).getByRole("button", { name: k.actions.edit }));
    const form = within(await screen.findByRole("dialog"));
    await form.findByDisplayValue(initial.name);
    return form;
  };
  const form = await openEdit();
  const advanced = form.getByRole("button", { name: k.drawer.advanced });
  await user.click(advanced);
  const runtime = form.getByLabelText(field(k.drawer.runtimeClass)) as HTMLInputElement;
  const secret = form.getByLabelText(field(k.drawer.imagePullSecret)) as HTMLInputElement;
  await user.clear(runtime);
  await user.type(runtime, "draft-runtime");
  await user.clear(secret);
  await user.type(secret, "draft-secret");
  // A concurrent server-side image edit is not this user's edit. The poll
  // updates the page but must not reset the draft or send that old image back.
  server.clusters = [{ ...initial, agent_image: "registry.example/agent:remote-edit", node_count: 4 }];
  const before = server.reads.clusters;
  await act(async () => { for (const tick of polls.values()) tick(); });
  await waitFor(() => assert.ok(server.reads.clusters > before, "the actual page reread its cluster list"));
  assert.equal(advanced.getAttribute("aria-expanded"), "true", "Advanced stays expanded after the 15-second response");
  assert.equal(runtime.value, "draft-runtime", "the runtime draft survived");
  assert.equal(secret.value, "draft-secret", "the registry-secret draft survived");
  assert.equal((form.getByLabelText(field(k.drawer.agentImage)) as HTMLInputElement).value, "registry.example/agent:owned", "a background response does not replace draft values");
  await user.click(form.getByRole("button", { name: k.drawer.save }));
  await waitFor(() => assert.equal(updates.length, 1, "one save request"));
  assert.deepEqual(updates[0], { uuid: initial.uuid, body: { runtime_class: "draft-runtime", image_pull_secret: "draft-secret" } }, "untouched image/source fields are omitted against the edit-session baseline");
  await waitFor(() => assert.ok(!screen.queryByRole("dialog"), "the successful edit closed"));
  const reopened = await openEdit();
  const reopenedAdvanced = reopened.getByRole("button", { name: k.drawer.advanced });
  assert.equal(reopenedAdvanced.getAttribute("aria-expanded"), "false", "a new opening initializes a new form");
  await user.click(reopenedAdvanced);
  assert.equal((reopened.getByLabelText(field(k.drawer.runtimeClass)) as HTMLInputElement).value, "draft-runtime");
  assert.equal((reopened.getByLabelText(field(k.drawer.agentImage)) as HTMLInputElement).value, "registry.example/agent:remote-edit", "reopening reads the latest saved entry");
});

import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { createI18n, dom } from "./helpers/dom.ts";
import { I18nextProvider } from "react-i18next";
import { list } from "./helpers/listPage.ts";

const { i18n, locale } = await createI18n(["kubernetes"]);
const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { api } = await import("../src/api.ts");
const { ConfigProvider } = await import("antd");
const { APIError } = await import("../src/apiError.ts");
const { default: KubernetesPage } = await import("../src/pages/KubernetesPage.tsx");
const { cardReturnText } = await import("../src/kubeClusters.ts");
import type { KubeCluster } from "../src/api";

const k = locale.kubernetes;
const originals = { ...api };
afterEach(() => { cleanup(); Object.assign(api, originals); });

const APPLY = "kubectl apply -f 'https://gpuworks.example/kube/manifest.yaml?token=secret-token'";
function cluster(overrides: Partial<KubeCluster> = {}): KubeCluster {
  return {
    id: 1, uuid: "c-1", name: "prod-gpu", namespace: "gpuworks-system", server_url: "https://gpuworks.example",
    agent_image: "", node_selector: "nvidia.com/gpu.present=true", runtime_class: "nvidia",
    containerd_socket: "/run/containerd/containerd.sock", engine_ports: "40000-42767", idle_return_seconds: 600,
    cluster_uid: "7f3c2a10-aaaa-bbbb-cccc-0123456789ab", revoked: false, node_count: 3,
    created_at: "2026-10-19T08:30:00Z", apply_command: APPLY, ...overrides,
  };
}
const prod = cluster();
const lab = cluster({ id: 2, uuid: "c-2", name: "lab", namespace: "gpu-lab", cluster_uid: "", node_count: 0, idle_return_seconds: -1, apply_command: "kubectl apply -f 'https://gpuworks.example/kube/manifest.yaml?token=lab-token'" });
const old = cluster({ id: 3, uuid: "c-3", name: "retired", revoked: true, idle_return_seconds: 7200 });

type Calls = Record<string, unknown[][]>;
function stubApi(options: { clusters?: KubeCluster[] | Error; admin?: boolean } = {}) {
  const calls: Calls = {};
  const record = (name: string, result: unknown) => async (...args: unknown[]) => {
    (calls[name] ??= []).push(args);
    if (result instanceof Error) throw result;
    return typeof result === "function" ? (result as (...a: unknown[]) => unknown)(...args) : result;
  };
  const clusters = options.clusters ?? [prod, lab, old];
  api.me = record("me", { role: options.admin === false ? "viewer" : "admin" }) as never;
  api.kubeClusters = record("kubeClusters", clusters instanceof Error ? clusters : list(clusters.map((c) => options.admin === false ? { ...c, apply_command: undefined } : c))) as never;
  api.kubeManifest = record("kubeManifest", "apiVersion: apps/v1\nkind: DaemonSet\n") as never;
  api.revokeKubeCluster = record("revokeKubeCluster", (uuid: string) => ({ ...prod, uuid, revoked: true })) as never;
  api.deleteKubeCluster = record("deleteKubeCluster", undefined) as never;
  api.createKubeCluster = record("createKubeCluster", (body: { name: string }) => cluster({ id: 9, uuid: "c-9", name: body.name, cluster_uid: "", node_count: 0 })) as never;
  return calls;
}

function renderPage() {
  const view = render(
    <ConfigProvider theme={{ token: { motion: false } }}><I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/kubernetes"]}><KubernetesPage /></MemoryRouter>
    </I18nextProvider></ConfigProvider>,
  );
  return { view, user: userEvent.setup({ document: dom.window.document }) };
}
const rowOf = async (name: string) => (await screen.findByText(name)).closest("tr") as HTMLElement;
const confirm = async (user: ReturnType<typeof userEvent.setup>, label: string) => {
  const ok = await waitFor(() => {
    const found = [...document.querySelectorAll(".ant-popover:not(.ant-popover-hidden)")]
      .flatMap((pop) => within(pop as HTMLElement).queryAllByRole("button", { name: new RegExp(`${label}$`) }));
    assert.ok(found.length > 0);
    return found.at(-1) as HTMLElement;
  });
  await user.click(ok);
};
const add = k.page.add;
const dialog = () => within(document.querySelector(".ant-drawer") as HTMLElement);

test("while loading it shows a spinner; an empty system invites connecting the first cluster; a failed load offers retry", async () => {
  let release: (rows: unknown) => void = () => {};
  stubApi();
  api.kubeClusters = (() => new Promise((resolve) => { release = resolve as never; })) as never;
  const first = renderPage();
  assert.ok(document.querySelector(".ant-spin-spinning"), "a spinner while the first load runs");
  release(list([]));
  assert.ok(await screen.findByText(k.page.empty));
  assert.ok(screen.getAllByRole("button", { name: new RegExp(`${add}$`) }).length >= 2, "header and empty state both offer connecting");
  first.view.unmount();

  const calls = stubApi({ clusters: new Error("boom") });
  const second = renderPage();
  assert.ok(await screen.findByText("boom"));
  const before = calls.kubeClusters.length;
  await second.user.click(screen.getByRole("button", { name: locale.common.actions.retry }));
  await waitFor(() => assert.ok(calls.kubeClusters.length > before));
});

test("the header explains how a cluster joins", async () => {
  stubApi();
  renderPage();
  assert.ok(await screen.findByText(k.page.description));
});

test("one row per cluster: namespace, node count, cluster UID or not-connected, card return policy, status and creation time", async () => {
  stubApi();
  renderPage();
  const row = await rowOf("prod-gpu");
  assert.equal(screen.getAllByRole("row").length, 1 + 3, "header plus a row per cluster");
  assert.ok(within(row).getByText("gpuworks-system"));
  assert.ok(within(row).getByText("3"));
  assert.ok(within(row).getByText("7f3c2a10-aaaa-bbbb-cccc-0123456789ab"));
  assert.ok(within(row).getByText("Returned after 10 min idle"));
  assert.ok(within(row).getByText(k.status.active));
  assert.ok(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$/.test(within(row).getByText(/^\d{4}-\d{2}-\d{2} /).textContent ?? ""));

  const fresh = await rowOf("lab");
  assert.ok(within(fresh).getByText(k.clusterUid.notEnrolled));
  assert.ok(within(fresh).getByText(k.returnPolicy.keep));
  assert.ok(within(await rowOf("retired")).getByText(k.status.revoked));
  assert.ok(within(await rowOf("retired")).getByText("Returned after 2 h idle"));
});

test("the return policy reads in seconds, minutes or hours, whichever divides evenly", () => {
  const t = i18n.getFixedT("en", "kubernetes");
  assert.equal(cardReturnText(t, 45), "Returned after 45 s idle");
  assert.equal(cardReturnText(t, 90), "Returned after 90 s idle");
  assert.equal(cardReturnText(t, 600), "Returned after 10 min idle");
  assert.equal(cardReturnText(t, 3600), "Returned after 1 h idle");
  assert.equal(cardReturnText(t, 0), "Returned after 0 s idle");
  assert.equal(cardReturnText(t, -1), "Kept (never returned)");
});

test("a viewer sees the clusters without the connect button, the actions column or the token", async () => {
  stubApi({ admin: false });
  renderPage();
  await rowOf("prod-gpu");
  await waitFor(() => assert.ok(screen.queryByRole("button", { name: new RegExp(`${add}$`) }) === null));
  assert.ok(screen.queryByRole("button", { name: k.actions.copyCommand }) === null);
  assert.ok(screen.queryByRole("button", { name: k.actions.delete }) === null);
  assert.equal(document.body.textContent?.includes("secret-token"), false);
});

test("an admin copies a cluster's connect command from its row; a revoked cluster cannot", async () => {
  stubApi();
  const { user } = renderPage();
  const row = await rowOf("prod-gpu");
  await user.click(within(row).getByRole("button", { name: k.actions.copyCommand }));
  assert.equal(await navigator.clipboard.readText(), APPLY);
  const revoked = within(await rowOf("retired")).getByRole("button", { name: k.actions.copyCommand }) as HTMLButtonElement;
  assert.ok(revoked.disabled, "a revoked token connects nothing");
});

test("the manifest opens in a dialog with a copy button; a failed load says so", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await user.click(within(await rowOf("prod-gpu")).getByRole("button", { name: k.actions.manifest }));
  const modal = within(await screen.findByRole("dialog"));
  assert.ok(await modal.findByText(/kind: DaemonSet/));
  assert.deepEqual(calls.kubeManifest, [["c-1"]]);
  await user.click(modal.getByRole("button", { name: k.manifest.copy }));
  assert.equal(await navigator.clipboard.readText(), "apiVersion: apps/v1\nkind: DaemonSet\n");
  await user.click(within(document.querySelector(".ant-modal-footer") as HTMLElement).getByRole("button", { name: k.manifest.close }));
  await waitFor(() => assert.ok(document.querySelector(".ant-modal-wrap:not([style*='display: none'])") === null));

  api.kubeManifest = (async () => { throw new Error("manifest 404"); }) as never;
  await user.click(within(await rowOf("lab")).getByRole("button", { name: k.actions.manifest }));
  const failed = within(await screen.findByRole("dialog"));
  assert.ok(await failed.findByText(k.manifest.failed));
  assert.ok(failed.getByText("manifest 404"));
});

test("revoking asks first, calls the API for that cluster only on confirm and reloads the list", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  const row = await rowOf("prod-gpu");
  await user.click(within(row).getByRole("button", { name: k.actions.revoke }));
  await screen.findByText(k.actions.revokeConfirm.replace("{{name}}", "prod-gpu"));
  assert.ok(screen.getByText(k.actions.revokeHelp));
  assert.equal(calls.revokeKubeCluster, undefined, "asking is not revoking");
  const before = calls.kubeClusters.length;
  await confirm(user, k.actions.revoke);
  await waitFor(() => assert.deepEqual(calls.revokeKubeCluster, [["c-1"]]));
  await waitFor(() => assert.ok(calls.kubeClusters.length > before));
  const revokedRow = within(await rowOf("retired")).getByRole("button", { name: k.actions.revoke }) as HTMLButtonElement;
  assert.ok(revokedRow.disabled, "an already revoked cluster offers no second revoke");
});

test("cancelling the revoke confirmation calls nothing", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await user.click(within(await rowOf("prod-gpu")).getByRole("button", { name: k.actions.revoke }));
  await confirm(user, k.actions.cancel);
  assert.equal(calls.revokeKubeCluster, undefined);
});

test("deleting asks first, calls the API on confirm and reloads", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await user.click(within(await rowOf("lab")).getByRole("button", { name: k.actions.delete }));
  await screen.findByText(k.actions.deleteConfirm.replace("{{name}}", "lab"));
  assert.equal(calls.deleteKubeCluster, undefined);
  const before = calls.kubeClusters.length;
  await confirm(user, k.actions.delete);
  await waitFor(() => assert.deepEqual(calls.deleteKubeCluster, [["c-2"]]));
  await waitFor(() => assert.ok(calls.kubeClusters.length > before));
  assert.ok(screen.queryByText(k.actions.failed) === null);
});

test("a cluster that still has nodes cannot be deleted: the page shows the product error and the row stays", async () => {
  const calls = stubApi();
  api.deleteKubeCluster = (async (...args: unknown[]) => {
    (calls.deleteKubeCluster ??= []).push(args);
    throw new APIError("KUBE_CLUSTER_HAS_NODES", undefined, "cluster has nodes", 409);
  }) as never;
  const { user } = renderPage();
  await user.click(within(await rowOf("prod-gpu")).getByRole("button", { name: k.actions.delete }));
  await confirm(user, k.actions.delete);
  const alert = await screen.findByRole("alert");
  assert.ok(alert.textContent?.includes(k.actions.failed));
  assert.ok(alert.textContent?.includes(locale.common.apiError.KUBE_CLUSTER_HAS_NODES), alert.textContent ?? "");
  assert.ok(screen.getByText("prod-gpu"), "the cluster is still listed");
});

test("connecting opens a form with the server address prefilled and the defaults in the advanced section", async () => {
  stubApi();
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  const form = dialog();
  assert.equal((await form.findByLabelText(k.drawer.serverUrl) as HTMLInputElement).value, "http://localhost", "the origin of this page, stated explicitly");
  assert.ok(form.getByPlaceholderText("nvidia.com/gpu.present=true"));
  assert.equal((form.getByLabelText(k.drawer.namespace) as HTMLInputElement).value, "gpuworks-system");
  assert.equal((form.getByLabelText(k.drawer.runtimeClass) as HTMLInputElement).value, "nvidia");
  assert.equal((form.getByLabelText(k.drawer.containerdSocket) as HTMLInputElement).value, "/run/containerd/containerd.sock");
  assert.equal((form.getByLabelText(k.drawer.enginePorts) as HTMLInputElement).value, "40000-42767");
  assert.equal((form.getByLabelText(k.drawer.idleReturnSeconds) as HTMLInputElement).value, "600");
  assert.ok(form.getByText(k.drawer.containerdSocketHelp), "the k3s hint");
  assert.ok(form.getByText(k.drawer.enginePortsHelp), "the NodePort hint");
});

test("a form without a name or with a server address that is no URL is refused before any request", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  const form = dialog();
  await user.click(await form.findByRole("button", { name: k.drawer.submit }));
  assert.ok(await form.findByText(k.drawer.nameRequired));
  await user.type(form.getByLabelText(k.drawer.name), "edge");
  await user.clear(form.getByLabelText(k.drawer.serverUrl));
  await user.type(form.getByLabelText(k.drawer.serverUrl), "gpuworks.example");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  assert.ok(await form.findByText(k.drawer.serverUrlInvalid));
  assert.equal(calls.createKubeCluster, undefined);
});

test("submitting sends exactly what the form shows, then presents the command to run in the cluster and reloads the list", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  const form = dialog();
  await user.type(await form.findByLabelText(k.drawer.name), "edge");
  await user.type(form.getByPlaceholderText("nvidia.com/gpu.present=true"), "gpu=a100,zone=b");
  const before = calls.kubeClusters.length;
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  assert.deepEqual(calls.createKubeCluster[0], [{
    name: "edge", server_url: "http://localhost", runtime_class: "nvidia", idle_return_seconds: 600,
    namespace: "gpuworks-system", node_selector: "gpu=a100,zone=b",
    containerd_socket: "/run/containerd/containerd.sock", engine_ports: "40000-42767",
  }]);
  assert.ok(await dialog().findByText(k.created.note));
  assert.ok(dialog().getByText(k.created.title.replace("{{name}}", "edge")));
  await waitFor(() => assert.ok(calls.kubeClusters.length > before));
  await user.click(dialog().getByRole("button", { name: k.created.copyCommand }));
  assert.equal(await navigator.clipboard.readText(), APPLY);
  assert.ok(dialog().getByLabelText(k.created.command).textContent === APPLY, "the command is shown for reading too");
});

test("advanced choices travel: no RuntimeClass is an explicit empty string, a resident cluster is -1", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  const form = dialog();
  await user.type(await form.findByLabelText(k.drawer.name), "k3s");
  await user.click(form.getByText(k.drawer.advanced));
  await user.clear(form.getByLabelText(k.drawer.runtimeClass));
  await user.clear(form.getByLabelText(k.drawer.containerdSocket));
  await user.type(form.getByLabelText(k.drawer.containerdSocket), "/run/k3s/containerd/containerd.sock");
  await user.click(form.getByRole("switch", { name: k.drawer.keepCards }));
  assert.ok((form.getByLabelText(k.drawer.idleReturnSeconds) as HTMLInputElement).disabled, "no seconds to enter once cards are kept");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  const body = calls.createKubeCluster[0][0] as Record<string, unknown>;
  assert.equal(body.runtime_class, "");
  assert.equal(body.idle_return_seconds, -1);
  assert.equal(body.containerd_socket, "/run/k3s/containerd/containerd.sock");
  assert.equal("node_selector" in body, false, "an empty selector is left to the server");
  assert.equal("agent_image" in body, false, "an empty image takes the default");
});

test("a refused cluster shows the reason at the field; a taken name shows at the name", async () => {
  stubApi();
  api.createKubeCluster = (async (body: { name: string }) => {
    if (body.name === "prod-gpu") throw new APIError("KUBE_CLUSTER_NAME_TAKEN", { name: "prod-gpu" }, "name taken", 409);
    throw new APIError("KUBE_CLUSTER_INVALID", { field: "engine_ports", reason: "overlaps the NodePort range" }, "invalid", 400);
  }) as never;
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  const form = dialog();
  await user.type(await form.findByLabelText(k.drawer.name), "prod-gpu");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  assert.ok(await form.findByText(locale.common.apiError.KUBE_CLUSTER_NAME_TAKEN.replace("{{name}}", "prod-gpu")));
  assert.ok(form.queryByText(k.created.note) === null, "nothing was created");

  await user.clear(form.getByLabelText(k.drawer.name));
  await user.type(form.getByLabelText(k.drawer.name), "other");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  assert.ok(await form.findByText(locale.common.apiError.KUBE_CLUSTER_INVALID.replace("{{field}}", "engine_ports").replace("{{reason}}", "overlaps the NodePort range")));
});

test("the drawer works from the keyboard: the name is focused on open, Enter submits, Escape closes", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  const form = dialog();
  const name = await form.findByLabelText(k.drawer.name);
  await waitFor(() => assert.ok(document.activeElement === name, "focus starts in the first field"));
  await user.keyboard("kbd{Enter}");
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  assert.equal((calls.createKubeCluster[0][0] as { name: string }).name, "kbd");
  await dialog().findByText(k.created.note);
  fireEvent.keyDown(document.querySelector(".ant-drawer-content-wrapper") as HTMLElement, { key: "Escape", keyCode: 27 });
  await waitFor(() => assert.ok(document.querySelector(".ant-drawer-open") === null));
});

test("Escape closes the form without sending anything", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  await dialog().findByLabelText(k.drawer.name);
  fireEvent.keyDown(document.querySelector(".ant-drawer-content-wrapper") as HTMLElement, { key: "Escape", keyCode: 27 });
  await waitFor(() => assert.ok(document.querySelector(".ant-drawer-open") === null));
  assert.equal(calls.createKubeCluster, undefined);
});

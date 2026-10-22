import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { createI18n, dom } from "./helpers/dom.ts";
import { I18nextProvider } from "react-i18next";
import { list } from "./helpers/listPage.ts";

const { i18n, locale } = await createI18n(["kubernetes", "providers"]);
const { MemoryRouter } = await import("react-router-dom");
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { api } = await import("../src/api.ts");
const { ConfigProvider } = await import("antd");
const { APIError } = await import("../src/apiError.ts");
const { default: ProviderClustersTable } = await import("../src/components/ProviderClustersTable.tsx");
const { default: ProviderInstancesTable } = await import("../src/components/ProviderInstancesTable.tsx");
const { default: KubernetesPage } = await import("../src/pages/KubernetesPage.tsx");
import type { KubeCluster, KubeClusterOperation, ProviderCluster, ProviderInstance } from "../src/api";

const k = locale.kubernetes;
const p = locale.providers;
const originals = { ...api };
afterEach(() => { cleanup(); Object.assign(api, originals); });

const gpuCluster: ProviderCluster = {
  external_id: "c-gpu", name: "ack-gpu", region: "cn-shanghai", version: "1.35.7-aliyun.1", state: "running", running: true, public_api: true,
  gpu_pools: [{ id: "np-l20", name: "l20", gpu_model: "NVIDIA L20", gpu_count: 1, nodes: 2, vendor: "nvidia", node_selector: "alibabacloud.com/nodepool-id=np-l20" }],
};
const privateCluster: ProviderCluster = { ...gpuCluster, external_id: "c-private", name: "ack-private", public_api: false, gpu_pools: [] };
const connectedCluster: ProviderCluster = { ...gpuCluster, external_id: "c-done", name: "ack-done", entry: { uuid: "e-1", name: "ack-done" } };
const newCluster: ProviderCluster = { ...gpuCluster, external_id: "c-new", name: "ack-new", running: false, state: "initial", gpu_pools: [] };

function entry(overrides: Partial<KubeCluster> = {}): KubeCluster {
  return {
    id: 5, uuid: "e-5", name: "ack-gpu", namespace: "gpuworks-system", server_url: "http://localhost", agent_image: "", node_selector: "alibabacloud.com/nodepool-id=np-l20",
    vendor: "nvidia", state_dir: "/opt/gpuworks", runtime_class: "", image_pull_secret: "", containerd_socket: "", api_server: "", engine_ports: "40000-42767",
    idle_return_seconds: 60, cluster_uid: "", revoked: false, node_count: 0, created_at: new Date().toISOString(),
    apply_command: "kubectl apply -f 'http://localhost/kube/manifest.yaml?token=t'",
    source: { provider_id: 3, provider_name: "ali-prod", provider_kind: "aliyun", cluster_id: "c-gpu" }, ...overrides,
  };
}

const plan: KubeClusterOperation = {
  dry_run: true,
  results: [
    { kind: "Namespace", name: "gpuworks-system", action: "create" },
    { kind: "DaemonSet", name: "gpuworks-agent", namespace: "gpuworks-system", action: "dependent" },
    { kind: "PriorityClass", name: "gpuworks-inference", action: "update" },
  ],
};

function wrap(node: React.ReactNode) {
  return render(
    <ConfigProvider theme={{ token: { motion: false } }}><I18nextProvider i18n={i18n}>
      <MemoryRouter>{node}</MemoryRouter>
    </I18nextProvider></ConfigProvider>,
  );
}
const user = () => userEvent.setup({ document: dom.window.document });
const modal = () => within(document.querySelector(".ant-modal-content") as HTMLElement);

test("a cloud account lists its clusters: GPU pools to connect, ones already connected, private APIs and clusters not ready", async () => {
  const calls: number[] = [];
  api.providerClusters = (async (id: number) => { calls.push(id); return list([gpuCluster, privateCluster, connectedCluster, newCluster]); }) as never;
  wrap(<ProviderClustersTable providerId={3} providerName="ali-prod" />);
  const row = (await screen.findByText("ack-gpu")).closest("tr") as HTMLElement;
  assert.deepEqual(calls, [3]);
  assert.ok(within(row).getByText("l20 · NVIDIA L20 ×1 · 2 nodes"));
  assert.ok(within(row).getByText(p.clusters.publicAPI));
  assert.ok(within(row).getByRole("button", { name: p.clusters.connect }));
  const priv = screen.getByText("ack-private").closest("tr") as HTMLElement;
  assert.ok(within(priv).getByText(p.clusters.privateAPI));
  assert.ok(within(priv).getByText(p.clusters.noGPUPools));
  const done = screen.getByText("ack-done", { selector: "strong" }).closest("tr") as HTMLElement;
  assert.ok(within(done).getByText(p.clusters.connected));
  assert.equal(within(done).queryByRole("button", { name: p.clusters.connect }), null, "connected once only");
  assert.ok(within(screen.getByText("ack-new").closest("tr") as HTMLElement).getByText(p.clusters.notRunning));
});

test("a failed listing says why and offers a refresh", async () => {
  let n = 0;
  api.providerClusters = (async () => { n++; if (n === 1) throw new Error("aliyun: Forbidden.RAM cs:DescribeClustersV1"); return list([]); }) as never;
  wrap(<ProviderClustersTable providerId={3} providerName="ali-prod" />);
  assert.ok(await screen.findByText("aliyun: Forbidden.RAM cs:DescribeClustersV1"));
  await user().click(screen.getByRole("button", { name: p.clusters.refresh }));
  assert.ok(await screen.findByText(p.clusters.empty));
});

test("connecting a pool opens the form filled from the cluster, records the source, then applies only after a checked plan is confirmed", async () => {
  api.providerClusters = (async () => list([gpuCluster])) as never;
  const created: unknown[] = [];
  api.createKubeCluster = (async (body: unknown) => { created.push(body); return entry(); }) as never;
  const ops: [string, string, boolean][] = [];
  api.kubeClusterOperation = (async (uuid: string, op: string, dryRun: boolean) => {
    ops.push([uuid, op, dryRun]);
    return dryRun ? plan : { dry_run: false, results: plan.results.map((r) => ({ ...r, action: "create" })) };
  }) as never;
  const u = user();
  wrap(<ProviderClustersTable providerId={3} providerName="ali-prod" />);
  await u.click(await screen.findByRole("button", { name: p.clusters.connect }));
  const drawer = within(document.querySelector(".ant-drawer") as HTMLElement);
  assert.ok(await drawer.findByText(k.drawer.fromSource.replace("{{source}}", "ali-prod · ack-gpu (c-gpu)")));
  assert.equal((drawer.getByLabelText(k.drawer.name) as HTMLInputElement).value, "ack-gpu");
  assert.equal((drawer.getByPlaceholderText("nvidia.com/gpu.present=true") as HTMLInputElement).value, "alibabacloud.com/nodepool-id=np-l20");
  await u.click(drawer.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(created.length, 1));
  assert.deepEqual(created[0], {
    name: "ack-gpu", server_url: "http://localhost", vendor: "nvidia", idle_return_seconds: 60, provider_id: 3, provider_cluster_id: "c-gpu",
    namespace: "gpuworks-system", node_selector: "alibabacloud.com/nodepool-id=np-l20", engine_ports: "40000-42767",
  });
  assert.ok(await drawer.findByText(k.created.byGpuworks));
  assert.ok(drawer.getByText(k.created.byKubectl), "kubectl stays an option");
  assert.equal(ops.length, 0, "nothing touches the cluster before the operator asks");

  await u.click(drawer.getByRole("button", { name: k.created.applyNow }));
  assert.ok(await modal().findByText(k.operation.checkedApply));
  assert.deepEqual(ops, [["e-5", "apply", true]], "a dry run first");
  assert.ok(modal().getByText("gpuworks-system/gpuworks-agent"));
  assert.ok(modal().getByText(k.operation.plan.create));
  assert.ok(modal().getByText(k.operation.plan.dependent));
  assert.ok(modal().getByText(k.operation.applyIntro.replace("{{account}}", "ali-prod").replace("{{cluster}}", "c-gpu")));
  await u.click(modal().getByRole("button", { name: k.operation.confirmApply }));
  assert.ok(await modal().findByText(k.operation.applied));
  assert.deepEqual(ops, [["e-5", "apply", true], ["e-5", "apply", false]]);
  assert.equal(modal().getAllByText(k.operation.done.create).length, 3);
});

test("a refused check names the step and the cluster's reason, and nothing can be confirmed", async () => {
  api.kubeClusters = (async () => list([entry()])) as never;
  api.me = (async () => ({ role: "admin" })) as never;
  const ops: boolean[] = [];
  api.kubeClusterOperation = (async (_u: string, _o: string, dryRun: boolean) => {
    ops.push(dryRun);
    return {
      dry_run: true, code: "KUBE_CLUSTER_OPERATION_FAILED", error: "Kubernetes API server: forbidden",
      params: { step: "api_server", reason: "clusterroles is forbidden: User \"ram-user\" cannot patch" },
      results: [{ kind: "Namespace", name: "gpuworks-system", action: "create" }, { kind: "ClusterRole", name: "gpuworks-agent-gpuworks-system", action: "failed", error: "forbidden" }],
    };
  }) as never;
  const u = user();
  wrap(<KubernetesPage />);
  const row = (await screen.findByText("ack-gpu", { selector: "strong" })).closest("tr") as HTMLElement;
  assert.ok(within(row).getByText("ali-prod"), "the source account");
  assert.ok(within(row).getByText("c-gpu"));
  await u.click(within(row).getByRole("button", { name: k.actions.apply }));
  assert.ok(await modal().findByText(k.operation.failed.api_server));
  assert.ok(modal().getByText("clusterroles is forbidden: User \"ram-user\" cannot patch"));
  assert.equal(modal().queryByRole("button", { name: k.operation.confirmApply }), null);
  await u.click(modal().getByRole("button", { name: k.operation.retry }));
  await waitFor(() => assert.equal(ops.length, 2));
  assert.deepEqual(ops, [true, true], "only dry runs");
});

test("removing from the cluster checks first, then deletes on confirmation; an entry made by hand offers neither", async () => {
  api.kubeClusters = (async () => list([entry(), entry({ id: 6, uuid: "e-6", name: "by-hand", source: undefined })])) as never;
  api.me = (async () => ({ role: "admin" })) as never;
  const ops: [string, string, boolean][] = [];
  api.kubeClusterOperation = (async (uuid: string, op: string, dryRun: boolean) => {
    ops.push([uuid, op, dryRun]);
    return { dry_run: dryRun, results: [{ kind: "DaemonSet", name: "gpuworks-agent", namespace: "gpuworks-system", action: "delete" },
      { kind: "PriorityClass", name: "gpuworks-inference", action: "kept", reason: "used by gpuworks in namespace gpuworks-npu" }] };
  }) as never;
  const u = user();
  wrap(<KubernetesPage />);
  const hand = (await screen.findByText("by-hand", { selector: "strong" })).closest("tr") as HTMLElement;
  assert.ok(within(hand).getByText(k.source.byHand));
  assert.equal(within(hand).queryByRole("button", { name: k.actions.remove }), null);
  assert.equal(within(hand).queryByRole("button", { name: k.actions.apply }), null);
  const row = screen.getByText("ack-gpu", { selector: "strong" }).closest("tr") as HTMLElement;
  await u.click(within(row).getByRole("button", { name: k.actions.remove }));
  assert.ok(await modal().findByText(k.operation.checkedRemove));
  assert.ok(modal().getByText("used by gpuworks in namespace gpuworks-npu"));
  await u.click(modal().getByRole("button", { name: k.operation.confirmRemove }));
  assert.ok(await modal().findByText(k.operation.removed));
  assert.deepEqual(ops, [["e-5", "remove", true], ["e-5", "remove", false]]);
});

test("a product error before any object (no source any more) is shown as such", async () => {
  api.kubeClusters = (async () => list([entry()])) as never;
  api.me = (async () => ({ role: "admin" })) as never;
  api.kubeClusterOperation = (async () => { throw new APIError("KUBE_CLUSTER_NO_SOURCE", undefined, "no source", 409); }) as never;
  const u = user();
  wrap(<KubernetesPage />);
  const row = (await screen.findByText("ack-gpu", { selector: "strong" })).closest("tr") as HTMLElement;
  await u.click(within(row).getByRole("button", { name: k.actions.apply }));
  assert.ok(await modal().findByText(locale.common.apiError.KUBE_CLUSTER_NO_SOURCE));
  assert.ok(modal().getByText(k.operation.failed.other));
});

test("a node of a managed cluster is marked as such and offers no install of its own", async () => {
  const base: ProviderInstance = { external_id: "i-1", name: "plain", gpu_model: "NVIDIA A10", gpu_count: 1, power: "running", price_per_hr: 0, region: "cn-shanghai",
    public_ip: "203.0.113.5", ssh_port: 22, ssh_user: "root", ssh_connections: [], kind: "vm", node_id: 0 };
  api.providerInstances = (async () => list([base, { ...base, external_id: "i-2", name: "ack-node", managed_cluster: "c-gpu" }])) as never;
  wrap(<ProviderInstancesTable providerId={3} refreshKey="1" />);
  const node = (await screen.findByText("ack-node")).closest("tr") as HTMLElement;
  assert.ok(within(node).getByText(p.instances.clusterNode));
  assert.equal(within(node).queryByRole("button", { name: p.instances.installAgent }), null);
  const plain = screen.getByText("plain").closest("tr") as HTMLElement;
  assert.ok(within(plain).getByRole("button", { name: p.instances.installAgent }));
});

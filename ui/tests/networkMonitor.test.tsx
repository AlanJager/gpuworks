import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { list } from "./helpers/listPage.ts";

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const anyContext = new Proxy(function () {} as never, {
  get: (_target: never, prop: string | symbol) => {
    if (prop === "canvas") return { width: 300, height: 200, style: {} };
    if (prop === "dpr") return 1;
    if (prop === "measureText") return () => ({ width: 10 });
    return anyContext;
  },
  set: () => true,
  apply: () => anyContext,
});
dom.window.HTMLCanvasElement.prototype.getContext = () => anyContext as CanvasRenderingContext2D;

const load = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const n = load("networks");
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en", fallbackLng: "en", resources: { en: { networks: n, common: load("common") } },
  defaultNS: "common", ns: ["networks", "common"], interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const { MemoryRouter } = await import("react-router-dom");
const apiModule = await import("../src/api.ts");
const monitor = await import("../src/pages/networks/NetworkMonitorPanel.tsx");
const NetworkMonitorPanel = monitor.default;
const NodeNetworksCard = (await import("../src/components/NodeNetworksCard.tsx")).default;
const { formatBitRate } = await import("../src/pages/networks/networkDisplay.tsx");

type API = typeof apiModule.api;
const originals: Partial<API> = {};
function stub<K extends keyof API>(key: K, impl: API[K]) {
  if (!(key in originals)) (originals as Record<string, unknown>)[key] = apiModule.api[key];
  apiModule.api[key] = impl;
}
after(() => { closeChannels(); dom.window.close(); });
afterEach(() => {
  cleanup();
  for (const [key, value] of Object.entries(originals)) (apiModule.api as Record<string, unknown>)[key] = value;
  for (const key of Object.keys(originals)) delete (originals as Record<string, unknown>)[key];
});

function wrap(element: React.ReactElement) {
  return render(<I18nextProvider i18n={i18n}><MemoryRouter>{element}</MemoryRouter></I18nextProvider>);
}

const pts = (...values: number[]) => values.map((value, i): [number, number] => [1_000 * (i + 1), value]);

function node(uuid: string, name: string, iface: string, hcas: string[] = []) {
  const rails = hcas.map((h) => ({ name: h, ip: "", netdev: "", link_layer: "infiniband", state: "ACTIVE", speed_gbps: 400 }));
  return { node_uuid: uuid, node_name: name, roles: ["compute"], attachment_id: 1, source: "node", interface: iface, ip: "10.0.0.1", hcas: rails, tools: [], warnings: [] };
}

function network(type: "host_rdma" | "ethernet", roles: string[], nodes: ReturnType<typeof node>[]) {
  return {
    id: 7, name: "fabric", description: "", roles, type, cidr: "", interface: "", builtin: false, attachments: [],
    resolved_node_count: nodes.length, warning_node_count: 0, created_at: "", updated_at: "",
    nodes, role_options: [], type_options: [], test_supported: true, latest_test: null,
  } as never;
}

test("per-node series keep only this network's interface and HCAs, summed per node", () => {
  const nodes = [node("a", "gpu-a", "bond0", ["mlx5_0:1", "mlx5_1:1"]), node("b", "gpu-b", "eth1", ["mlx5_0:1"])];
  const series = [
    { labels: { node: "a", hca: "mlx5_0:1" }, points: pts(10, 20) },
    { labels: { node: "a", hca: "mlx5_1:1" }, points: pts(1, 2) },
    { labels: { node: "a", hca: "mlx5_4:1" }, points: pts(500, 500) }, // storage NIC, not on this network
    { labels: { node: "b", hca: "mlx5_0:1" }, points: pts(7) },
    { labels: { node: "z", hca: "mlx5_0:1" }, points: pts(9) }, // a node not on the network
  ];
  const got = monitor.perNode(nodes as never, series, (nn, l) => nn.hcas.some((h) => h.name === l.hca));
  assert.deepEqual(got, [
    { labels: { node: "gpu-a" }, points: pts(11, 22) },
    { labels: { node: "gpu-b" }, points: pts(7) },
  ]);
  assert.deepEqual(monitor.perNode(nodes as never, undefined, () => true), []);
  assert.deepEqual(monitor.sumSeries([{ labels: {}, points: [[2, 1]] }, { labels: {}, points: [[1, 3], [2, 4]] }]), [[1, 3], [2, 5]]);
});

test("bit rates read like link speeds", () => {
  assert.equal(formatBitRate(0), "0 b/s");
  assert.equal(formatBitRate(125), "1.0 Kb/s");
  assert.equal(formatBitRate(1.5e9), "12.0 Gb/s");
  assert.equal(formatBitRate(5e10), "400 Gb/s");
  assert.equal(formatBitRate(2e12), "16.0 Tb/s");
});

test("an RDMA network charts NIC, RDMA, errors and its roles, queried once per metric", async () => {
  const calls: [string, Record<string, unknown>][] = [];
  stub("metricsQuery", async (metric, params = {}) => {
    calls.push([metric, params]);
    return list([]);
  });
  wrap(<NetworkMonitorPanel network={network("host_rdma", ["compute", "storage_sync"], [node("a", "gpu-a", "bond0", ["mlx5_0:1"])])} />);
  await screen.findByText(n.monitor.rdmaRx);
  for (const title of [n.monitor.nicRx, n.monitor.nicTx, n.monitor.rdmaTx, n.monitor.errors, n.monitor.role.storage_sync]) {
    assert.ok(screen.getByText(title), title);
  }
  assert.equal(screen.queryByText(n.monitor.role.serving), null, "the network has no serving role");
  assert.equal(screen.queryByText(n.monitor.computeEthernet), null);
  assert.equal(calls.length, 8);
  assert.ok(calls.every(([, p]) => p.range === "1h" && p.step === "30s" && p.node === undefined));

  await userEvent.click(screen.getByText("24h"));
  await waitFor(() => assert.equal(calls.length, 16));
  assert.ok(calls.slice(8).every(([, p]) => p.range === "24h" && p.step === "10m"));
});

test("an Ethernet compute network explains its traffic is inside the NIC totals", async () => {
  stub("metricsQuery", async () => list([]));
  wrap(<NetworkMonitorPanel network={network("host_ethernet", ["compute", "serving"], [node("a", "gpu-a", "eth0")])} />);
  await screen.findByText(n.monitor.nicRx);
  assert.equal(screen.queryByText(n.monitor.rdmaRx), null);
  assert.ok(screen.getByText(n.monitor.role.serving));
  assert.ok(screen.getByText(n.monitor.computeEthernet));
});

test("monitoring shows load errors and a network without nodes", async () => {
  stub("metricsQuery", async () => { throw new Error("tsdb unavailable"); });
  const { unmount } = wrap(<NetworkMonitorPanel network={network("host_ethernet", ["serving"], [node("a", "gpu-a", "eth0")])} />);
  assert.ok(await screen.findByText(/tsdb unavailable/));
  unmount();
  stub("metricsQuery", async () => list([]));
  wrap(<NetworkMonitorPanel network={network("host_ethernet", ["serving"], [])} />);
  assert.ok(await screen.findByText(n.detail.noNodes));
});

test("the node networks card shows each link's latest rates", async () => {
  stub("me", async () => ({ role: "member" }) as never);
  stub("nodeNetworks", async () => ({
    networks: [], serving_forward: null, inventory_reported: true,
    management: { interface: "", ip: "", roles_off: [], behind_nat: false },
    interfaces: [
      { name: "bond0", state: "up", mtu: 9000, speed_mbps: 200000, mac: "", addrs: [], rx_bytes_per_sec: 1.5e9, tx_bytes_per_sec: 2e8 },
      { name: "eth9", state: "down", mtu: 1500, speed_mbps: 0, mac: "", addrs: [] },
    ],
    rdma_ports: [{ device: "mlx5_0", port: 1, state: "ACTIVE", link_layer: "InfiniBand", rate_gbps: 400, netdev: "", rx_bytes_per_sec: 5e10, tx_bytes_per_sec: 0 }],
  }) as never);
  wrap(<NodeNetworksCard nodeUUID="uuid-a" />);
  assert.ok(await screen.findByText("12.0 Gb/s / 1.6 Gb/s"));
  assert.ok(screen.getByText("400 Gb/s / 0 b/s"));
  assert.ok(screen.getAllByRole("columnheader", { name: n.node.rate }).length >= 2, "a rate column on both tables");
  assert.equal(screen.getAllByText("—").length > 0, true, "a NIC with no recent sample shows a dash");
});

test("the node networks card lists the ports its accelerator cards own", async () => {
  stub("me", async () => ({ role: "member" }) as never);
  const base = {
    networks: [], serving_forward: null, inventory_reported: true,
    management: { interface: "", ip: "", roles_off: [], behind_nat: false },
    interfaces: [], rdma_ports: [],
  };
  stub("nodeNetworks", async () => ({
    ...base,
    accelerator_ports: [
      { name: "npu3", index: 3, ip: "172.20.9.59", netmask: "255.255.0.0", state: "UP", speed_gbps: 200 },
      { name: "npu6", index: 6, ip: "", netmask: "", state: "DOWN", speed_gbps: 0 },
    ],
  }));
  const { unmount } = wrap(<NodeNetworksCard nodeUUID="uuid-npu" />);
  const up = (await screen.findByText("npu3")).closest("tr") as HTMLElement;
  assert.ok(within(up).getByText("172.20.9.59 / 255.255.0.0"));
  assert.ok(within(up).getByText("UP"));
  assert.ok(within(up).getByText("200G"));
  const down = screen.getByText("npu6").closest("tr") as HTMLElement;
  assert.ok(within(down).getByText("DOWN"));
  assert.equal(within(down).getAllByText("—").length, 2, "no address and no speed");
  assert.ok(screen.getByText(n.node.accelerator));
  assert.ok(screen.getByRole("columnheader", { name: n.node.card.ip }));
  unmount();

  // A node whose cards have no port of their own shows no such table, and its
  // lone interface table carries no heading of its own under the card title.
  stub("nodeNetworks", async () => ({ ...base, accelerator_ports: [] }));
  wrap(<NodeNetworksCard nodeUUID="uuid-plain" />);
  assert.ok(await screen.findByRole("columnheader", { name: n.node.iface.name }));
  assert.equal(screen.queryByText(n.node.accelerator), null);
  assert.equal(screen.queryByText(n.node.interfaces), null);
});

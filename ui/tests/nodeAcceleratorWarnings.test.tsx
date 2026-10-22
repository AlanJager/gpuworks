import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n } = await createI18n(["nodes"], "zh");
const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { GPUProbeFailedTag } = await import("../src/components/NodeAcceleratorWarnings.tsx");
type NodeT = import("../src/api.ts").Node;

afterEach(() => cleanup());

const node = {
  uuid: "11111111-1111-1111-1111-111111111111",
  state: "online",
  hostname: "zstone-164",
  os: "linux", kernel: "4.18.0", arch: "amd64", cpu_model: "Xeon",
  cpu_cores: 96, mem_total_gib: 754, disk_total_gib: 307, gpu_count: 2,
  gpus: [],
} as unknown as NodeT;

const show = (n: NodeT) => render(<I18nextProvider i18n={i18n}>{<GPUProbeFailedTag node={n} />}</I18nextProvider>);

// A vendor tool that did not answer changes nothing about what the node
// is, only what is known about the cards it owns. The tag says which of
// the two happened, and names the tool's own error, because that is what
// the per-card finding tells an operator to go and read.
test("a vendor tool that did not answer is named, with its own error", async () => {
  show({ ...node, gpu_probe_error: "nvidia-smi: no answer within 10s", gpu_probe_failed_at: "2026-10-14T01:00:00Z" });
  const tag = screen.getByText("厂商工具未响应");
  const label = tag.closest("[aria-label]");
  assert.ok(label, "the detail is available to assistive technology");
  assert.match(label.getAttribute("aria-label") ?? "", /nvidia-smi: no answer within 10s/);
  fireEvent.focus(label);
  const tip = await waitFor(() => screen.getByRole("tooltip"));
  // It says what is stale and what is not: the bus answered.
  assert.match(tip.textContent ?? "", /本机自用的卡/);
  assert.match(tip.textContent ?? "", /PCI 总线/);
});

test("a node whose probe succeeds says nothing", () => {
  const { container } = show(node);
  assert.equal(container.textContent, "");
});

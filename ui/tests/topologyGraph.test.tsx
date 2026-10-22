import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import type { Topology } from "../src/api.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
	Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });

const en = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/nodes.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "en", defaultNS: "nodes", interpolation: { escapeValue: false }, resources: { en: { nodes: en } } });

const { cleanup, render, screen } = await import("@testing-library/react");
const { default: TopologyGraph } = await import("../src/components/TopologyGraph.tsx");

afterEach(() => cleanup());

// A node's topology as GET /api/v1/nodes/{uuid} serves it (dto.NodeTopology):
// snake_case keys, numa_groups keyed by the NUMA node as a JSON string. Two
// NVLink pairs on two NUMA nodes, SYS across them.
const link = (raw: string, cls: number, bonds = 0) => ({ raw, class: cls, nvlink_bonds: bonds });
const self = link("X", 0);
const nv = link("NV12", 5, 12);
const sys = link("SYS", 1);
const wire = JSON.parse(JSON.stringify({
	gpus: [
		{ index: 0, cpu_affinity: "0-23", numa_node: 0 },
		{ index: 1, cpu_affinity: "0-23", numa_node: 0 },
		{ index: 2, cpu_affinity: "24-47", numa_node: 1 },
		{ index: 3, cpu_affinity: "24-47", numa_node: 1 },
	],
	matrix: [
		[self, nv, sys, sys],
		[nv, self, sys, sys],
		[sys, sys, self, nv],
		[sys, sys, nv, self],
	],
	fast_cliques: [[0, 1], [2, 3]],
	nvlink_cliques: [[0, 1], [2, 3]],
	numa_groups: { 0: [0, 1], 1: [2, 3] },
})) as Topology;

test("the topology graph reads the snake_case wire topology: GPUs, NUMA groups and NVLink pairs", () => {
	render(<I18nextProvider i18n={i18n}><TopologyGraph topology={wire} /></I18nextProvider>);
	assert.ok(screen.getByText("GPU 4"));
	assert.ok(screen.getByText("NUMA 2"));
	assert.ok(screen.getByText("High-speed links 2"));
	assert.equal(screen.getAllByText("NV12").length, 2, "each NVLink pair is labelled with its bond count");
});

test("a topology with no GPUs, or none at all, says nothing was collected", () => {
	const { unmount } = render(<I18nextProvider i18n={i18n}><TopologyGraph topology={{ ...wire, gpus: [] }} /></I18nextProvider>);
	assert.ok(screen.getByText(en.topology.empty));
	unmount();
	render(<I18nextProvider i18n={i18n}><TopologyGraph /></I18nextProvider>);
	assert.ok(screen.getByText(en.topology.empty));
});

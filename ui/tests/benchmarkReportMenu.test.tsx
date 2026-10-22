import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
	Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
Object.defineProperty(globalThis, "ResizeObserver", { value: class { observe() {} unobserve() {} disconnect() {} }, configurable: true });

const i18n = createInstance();
await i18n.use(initReactI18next).init({
	lng: "zh", defaultNS: "evaluations", interpolation: { escapeValue: false },
	resources: { zh: { evaluations: { agentic: { export: { button: "导出报告", markdown: "下载 Markdown", csv: "下载 CSV", html: "下载独立 HTML（含图表）" } } } } },
});

const { cleanup, render, screen } = await import("@testing-library/react");
const { benchmarkReportDownloads, benchmarkReportURL, default: BenchmarkReportMenu } = await import("../src/pages/evaluations/BenchmarkReportMenu.tsx");

afterEach(() => cleanup());

test("report menu exposes Markdown, CSV, and standalone HTML downloads", () => {
	assert.equal(benchmarkReportURL(7, "markdown"), "/api/v1/benchmarks/7/report?format=markdown");
	assert.deepEqual(benchmarkReportDownloads(7), [
		{ format: "markdown", href: "/api/v1/benchmarks/7/report?format=markdown" },
		{ format: "csv", href: "/api/v1/benchmarks/7/report?format=csv" },
		{ format: "html", href: "/api/v1/benchmarks/7/report?format=html" },
	]);
	render(<I18nextProvider i18n={i18n}><BenchmarkReportMenu runID={7} /></I18nextProvider>);
	assert.ok(screen.getByRole("button", { name: /导出报告/ }));
});

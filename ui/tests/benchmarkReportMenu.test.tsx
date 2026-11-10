import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
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
// rc-overflow expects browser MessageChannel; JSDOM has none. Avoid Node ports keeping the test alive.
Object.defineProperty(globalThis, "MessageChannel", { value: undefined, configurable: true });
Object.defineProperty(globalThis, "ResizeObserver", { value: class { observe() {} unobserve() {} disconnect() {} }, configurable: true });

const i18n = createInstance();
await i18n.use(initReactI18next).init({
	lng: "zh", defaultNS: "evaluations", interpolation: { escapeValue: false },
	resources: { zh: { evaluations: { agentic: { export: { button: "导出报告", png: "下载 PNG 长图", exporting: "正在生成报告…", visualFailed: "图片导出失败", markdown: "下载 Markdown", csv: "下载 CSV", html: "下载离线 HTML（与页面同款）" } } } } },
});

const { message } = await import("antd");
const { cleanup, render, screen, fireEvent, waitFor, act } = await import("@testing-library/react");
const { benchmarkReportDownloads, benchmarkReportURL, default: BenchmarkReportMenu } = await import("../src/pages/evaluations/BenchmarkReportMenu.tsx");

afterEach(() => { cleanup(); act(() => message.destroy()); });
after(() => dom.window.close());

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

for (const format of ["png", "html"] as const) test(`${format} export supports keyboard selection and shows progress until complete`, async () => {
	let finish!: () => void;
	const calls: string[] = [];
	render(<I18nextProvider i18n={i18n}><BenchmarkReportMenu runID={7} onExportVisual={(value) => { calls.push(value); return new Promise<void>((resolve) => { finish = resolve; }); }} /></I18nextProvider>);
	fireEvent.click(screen.getByRole("button", { name: /导出报告/ }));
	const item = await screen.findByRole("menuitem", { name: format === "png" ? "下载 PNG 长图" : "下载离线 HTML（与页面同款）" });
	assert.equal(item.querySelector("a"), null, "visual reports must use the rendered page, not the legacy API template");
	item.focus();
	fireEvent.keyDown(item, { key: "Enter", code: "Enter", keyCode: 13, which: 13 });
	await waitFor(() => assert.deepEqual(calls, [format]));
	assert.ok(screen.getByRole("button", { name: /正在生成报告/ }));
	await act(async () => finish());
	assert.ok(screen.getByRole("button", { name: /导出报告/ }));
});

test("PNG export reports errors and permits another attempt", async () => {
	let calls = 0;
	render(<I18nextProvider i18n={i18n}><BenchmarkReportMenu runID={7} onExportVisual={async () => { calls++; throw new Error("canvas unavailable"); }} /></I18nextProvider>);
	for (let attempt = 1; attempt <= 2; attempt++) {
		fireEvent.click(screen.getByRole("button", { name: /导出报告/ }));
		fireEvent.click(await screen.findByRole("menuitem", { name: "下载 PNG 长图" }));
		await waitFor(() => assert.equal(calls, attempt));
		await waitFor(() => assert.ok(screen.getByRole("button", { name: /导出报告/ })));
	}
	assert.ok((await screen.findAllByText("图片导出失败")).length > 0);
});

test("PNG export is disabled while report data is loading", async () => {
	let calls = 0;
	render(<I18nextProvider i18n={i18n}><BenchmarkReportMenu runID={7} visualDisabled onExportVisual={async () => { calls++; }} /></I18nextProvider>);
	fireEvent.click(screen.getByRole("button", { name: /导出报告/ }));
	const item = await screen.findByRole("menuitem", { name: "下载 PNG 长图" });
	assert.equal(item.getAttribute("aria-disabled"), "true");
	fireEvent.click(item);
	assert.equal(calls, 0);
});


test("the page menu has one HTML export alongside PNG, Markdown, and CSV", async () => {
	render(<I18nextProvider i18n={i18n}><BenchmarkReportMenu runID={7} onExportVisual={async () => {}} /></I18nextProvider>);
	fireEvent.click(screen.getByRole("button", { name: /导出报告/ }));
	const items = await screen.findAllByRole("menuitem");
	assert.equal(items.length, 4);
	assert.deepEqual(items.map((item) => item.textContent), ["下载离线 HTML（与页面同款）", "下载 PNG 长图", "下载 Markdown", "下载 CSV"]);
	assert.equal(screen.getByRole("link", { name: "下载 Markdown" }).getAttribute("href"), "/api/v1/benchmarks/7/report?format=markdown");
	assert.equal(screen.getByRole("link", { name: "下载 CSV" }).getAttribute("href"), "/api/v1/benchmarks/7/report?format=csv");
	assert.equal(document.querySelector('a[href="/api/v1/benchmarks/7/report?format=html"]'), null);
});

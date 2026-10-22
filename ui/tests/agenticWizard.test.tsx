import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import type { BenchmarkMeta } from "../src/api.ts";
import { summarizeAgenticLoad } from "../src/pages/evaluations/agenticWorkload.ts";

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
	resources: { zh: { evaluations: { agentic: {
		meanInput: "平均输入长度", meanInputHelp: "平均 prompt 长度", recommended: "默认", summaryAria: "Agentic 负载摘要",
		metric: { peakPrompt: "最大输入", context: "需要准备的上下文", turns: "每段会话轮数" },
		result: { designReuse: "设计前缀复用率" }, turnShape: "每轮新增 {{input}} 输入 · 生成 {{output}} 输出",
		volume: "{{sessions}} 段会话 · {{requests}} 个请求",
	} } } },
});

const meta = {
	agentic_presets: [{ value: "agentic-80k-v1", label: "80K", mean_input_tokens: 79_998, first_turn_input_tokens: 74_160, peak_input_tokens: 85_836, required_context_tokens: 90_000, recommended: true }],
	agentic_defaults: { preset: "agentic-80k-v1", turns: 13, subsequent_turn_input_tokens: 753, output_tokens: 220, seed: 42, concurrency: [1, 2, 4, 8], sessions_per_point: { "1": 4, "2": 8, "4": 8, "8": 16 }, sessions_per_concurrency: 2, min_sessions_per_point: 4 },
} as BenchmarkMeta;

const { Form } = await import("antd");
const { cleanup, render, screen } = await import("@testing-library/react");
const { default: AgenticLoadSummary } = await import("../src/pages/evaluations/AgenticLoadSummary.tsx");

afterEach(() => cleanup());

test("compact Agentic load summary exposes context, turns, reuse, and request volume", () => {
	const summary = summarizeAgenticLoad(79_998, 13, meta);
	render(<I18nextProvider i18n={i18n}><Form initialValues={{ agentic_preset: "agentic-80k-v1", agentic_mean_input: 79_998 }}><AgenticLoadSummary summary={summary} meta={meta} onPresetChange={() => {}} /></Form></I18nextProvider>);
	assert.ok(screen.getByRole("region", { name: "Agentic 负载摘要" }));
	assert.ok(screen.getByText("90K"));
	assert.ok(screen.getByText("92.0%"));
	assert.ok(screen.getByText("36 段会话 · 468 个请求"));
	assert.equal(screen.queryByText(/自动推导/), null);
});

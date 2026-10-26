import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { tokenizerFormState, tokenizerReference, tokenizerSourceOrDefault } from "../src/pages/evaluations/tokenizerSource.ts";

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
	resources: { zh: { evaluations: { wizard: {
		tokenizerSource: "Tokenizer 来源", tokenizerEstimate: "估算", tokenizerAuto: "自动（模型仓库 / 目标机）", uploadFile: "上传文件",
		tokenizerAutoHelp: "优先使用目标机上已有的 tokenizer.json；发压点选 server 时经数据通道取回，两者都没有再回落到模型仓库。",
		tokenizerAgenticHelp: "Agentic 的多轮长度和设计前缀复用率都按 token 计算；估算误差会逐轮累积，并可能使长上下文超限，因此必须使用 tokenizer。",
		tokenizerControlledHelp: "按 token 长度必须提供可加载 tokenizer", tokenizerManagedHelp: "托管模型帮助", tokenizerHelp: "普通帮助", tokenizerRequired: "请上传 tokenizer.json",
		tokenizerRepositoryRequired: "请输入模型仓库", currentTokenizerFile: "当前文件：{{name}}",
		uploadTokenizer: "上传 tokenizer.json", modelRepository: "模型仓库", modelRepositoryPlaceholder: "例如 Qwen/Qwen3-8B",
	} } } },
});

const { Form } = await import("antd");
const { cleanup, render, screen } = await import("@testing-library/react");
const { default: TokenizerSourceFields } = await import("../src/pages/evaluations/TokenizerSourceFields.tsx");

afterEach(() => cleanup());

test("rerun restores every tokenizer source and rebuilds remote references", () => {
	assert.deepEqual(tokenizerFormState(), { source: "estimate" });
	assert.deepEqual(tokenizerFormState("hf://Qwen/Qwen3-8B"), { source: "huggingface", repository: "Qwen/Qwen3-8B", reference: "hf://Qwen/Qwen3-8B" });
	assert.deepEqual(tokenizerFormState("ms://Qwen/Qwen3-8B"), { source: "modelscope", repository: "Qwen/Qwen3-8B", reference: "ms://Qwen/Qwen3-8B" });
	assert.deepEqual(tokenizerFormState("/tmp/tokenizer.json"), { source: "upload", reference: "/tmp/tokenizer.json" });
	assert.equal(tokenizerReference("estimate", undefined, undefined, { huggingface: "hf", modelscope: "ms" }), undefined);
	assert.equal(tokenizerReference("huggingface", "Qwen/Qwen3-8B", undefined, { huggingface: "hf", modelscope: "ms" }), "hf://Qwen/Qwen3-8B");
	assert.equal(tokenizerReference("upload", undefined, "/tmp/tokenizer.json", { huggingface: "hf", modelscope: "ms" }), "/tmp/tokenizer.json");
});

test("a managed rerun re-resolves a machine-local path instead of reusing it", () => {
	// The path belonged to the node the previous run used, so a managed target
	// hands it back to the automatic source for resolution and validation.
	assert.deepEqual(tokenizerFormState("/models/qwen/tokenizer.json", true), { source: "auto", reference: "/models/qwen/tokenizer.json" });
	// An external endpoint has no machine to resolve on, so the file the server
	// holds stays an upload.
	assert.deepEqual(tokenizerFormState("/models/qwen/tokenizer.json", false), { source: "upload", reference: "/models/qwen/tokenizer.json" });
	assert.deepEqual(tokenizerFormState("hf://Qwen/Qwen3-8B", true), { source: "huggingface", repository: "Qwen/Qwen3-8B", reference: "hf://Qwen/Qwen3-8B" });
	assert.deepEqual(tokenizerFormState(undefined, true), { source: "estimate" });
	assert.equal(tokenizerReference("auto", undefined, undefined, {}), undefined);
});

test("the automatic source is the managed default and never survives an agentic estimate", () => {
	assert.equal(tokenizerSourceOrDefault(true, false, undefined, false), "auto");
	assert.equal(tokenizerSourceOrDefault(false, false, undefined, false), "estimate");
	assert.equal(tokenizerSourceOrDefault(true, false, "/models/qwen/tokenizer.json"), "auto");
	assert.equal(tokenizerSourceOrDefault(false, false, "/models/qwen/tokenizer.json"), "upload");
	// Agentic cannot estimate, so the form must start somewhere a tokenizer can
	// still be found.
	assert.equal(tokenizerSourceOrDefault(true, true, undefined, false), "auto");
	assert.equal(tokenizerSourceOrDefault(false, true, undefined, false), "huggingface");
	assert.equal(tokenizerSourceOrDefault(true, true, "hf://Qwen/Qwen3-8B"), "huggingface");
});

test("Agentic offers every source, disables estimation, and explains why", () => {
	render(<I18nextProvider i18n={i18n}><Form initialValues={{ tokenizer_source: "huggingface" }}><TokenizerSourceFields source="huggingface" dataset="agentic" targetKind="external" onUpload={() => {}} /></Form></I18nextProvider>);
	assert.equal(screen.getAllByRole("radio").length, 5);
	assert.equal((screen.getByRole("radio", { name: "估算" }) as HTMLInputElement).disabled, true);
	assert.ok(screen.getByText(/按 token 长度必须提供可加载/));
});

test("a managed target offers the automatic source, an external endpoint does not", () => {
	render(<I18nextProvider i18n={i18n}><Form initialValues={{ tokenizer_source: "auto" }}><TokenizerSourceFields source="auto" dataset="random" targetKind="instance" onUpload={() => {}} /></Form></I18nextProvider>);
	assert.equal((screen.getByRole("radio", { name: "自动（模型仓库 / 目标机）" }) as HTMLInputElement).disabled, false);
	assert.equal((screen.getByRole("radio", { name: "估算" }) as HTMLInputElement).disabled, true);
	assert.ok(screen.getByText(/优先使用目标机上已有的 tokenizer/));
	// Nothing to type: the source is resolved rather than entered.
	assert.equal(screen.queryByLabelText("模型仓库"), null);
	cleanup();

	render(<I18nextProvider i18n={i18n}><Form initialValues={{ tokenizer_source: "estimate" }}><TokenizerSourceFields source="estimate" dataset="random" targetKind="external" onUpload={() => {}} /></Form></I18nextProvider>);
	assert.equal((screen.getByRole("radio", { name: "自动（模型仓库 / 目标机）" }) as HTMLInputElement).disabled, true);
});

test("a repository source asks for the repository and nothing else", () => {
	render(<I18nextProvider i18n={i18n}><Form initialValues={{ tokenizer_source: "modelscope" }}><TokenizerSourceFields source="modelscope" dataset="random" targetKind="instance" onUpload={() => {}} /></Form></I18nextProvider>);
	assert.ok(screen.getByLabelText("模型仓库"));
	assert.equal(screen.queryByText(/上传 tokenizer.json$/), null);
});

test("an uploaded tokenizer remains visible when rerunning", () => {
	render(<I18nextProvider i18n={i18n}><Form initialValues={{ tokenizer_source: "upload", tokenizer_ref: "/var/lib/gpuworks/tokenizer-123.json" }}><TokenizerSourceFields source="upload" dataset="random" targetKind="external" tokenizerRef="/var/lib/gpuworks/tokenizer-123.json" onUpload={() => {}} /></Form></I18nextProvider>);
	assert.ok(screen.getByText("当前文件：tokenizer-123.json"));
});

 test("random token-length workloads disable estimation and require an uploaded tokenizer", () => {
 render(<I18nextProvider i18n={i18n}><Form initialValues={{ tokenizer_source: "upload" }}><TokenizerSourceFields source="upload" dataset="random" targetKind="external" onUpload={() => {}} /></Form></I18nextProvider>);
 assert.equal((screen.getByRole("radio", { name: "估算" }) as HTMLInputElement).disabled, true);
 assert.ok(screen.getByText(/按 token 长度必须提供可加载/));
 assert.equal(tokenizerSourceOrDefault(false, true, undefined, false), "huggingface");
 });

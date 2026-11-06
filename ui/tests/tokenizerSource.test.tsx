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
		tokenizerSource: "Tokenizer 来源", tokenizerEstimate: "估算", tokenizerAuto: "自动（推理服务分词接口 / 目标机 / 模型仓库）", uploadFile: "上传文件",
		tokenizerAutoHelp: "优先用推理服务自己的 /v1/tokenize、/v1/detokenize 生成负载，长度与引擎计数一致，任何 tokenizer 格式都适用；接口不可用时，用目标机上已有的 tokenizer.json 或 tiktoken.model（发压点选 server 时经数据通道取回），都没有再回落到模型仓库。",
		tokenizerAutoExternalHelp: "外部端点使用端点自己的 /v1/tokenize、/v1/detokenize；端点不提供时评测在推理前终止，请改选其他来源。",
		tokenizerEngine: "推理服务 (de)tokenize 接口", tokenizerEngineHelp: "用推理服务自己的 /v1/tokenize、/v1/detokenize 生成负载（vLLM、SGLang 提供）；接口不可用时评测在推理前终止，不会改用其他来源。",
		tokenizerAgenticHelp: "Agentic 的多轮长度和设计前缀复用率都按 token 计算；估算误差会逐轮累积，并可能使长上下文超限，因此必须使用 tokenizer。",
		tokenizerControlledHelp: "按 token 长度必须提供可加载 tokenizer", tokenizerManagedHelp: "托管模型帮助", tokenizerHelp: "普通帮助", tokenizerRequired: "请上传 tokenizer.json",
		tokenizerRepositoryRequired: "请输入模型仓库", currentTokenizerFile: "当前文件：{{name}}",
		uploadTokenizer: "上传 tokenizer.json", modelRepository: "模型仓库", modelRepositoryPlaceholder: "例如 Qwen/Qwen3-8B",
	} } } },
});

const { Form } = await import("antd");
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
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
	// The target's own tokenizer: alone it was chosen; with a fallback it was
	// what "auto" resolved to. Neither carries a reference from the form.
	assert.deepEqual(tokenizerFormState("engine:"), { source: "engine", reference: "engine:" });
	assert.deepEqual(tokenizerFormState("engine:hf://moonshotai/Kimi-K3.1-Pro", true), { source: "auto", reference: "engine:hf://moonshotai/Kimi-K3.1-Pro" });
	assert.equal(tokenizerReference("engine", undefined, undefined, { huggingface: "hf", modelscope: "ms" }), undefined);
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
	assert.equal(screen.getAllByRole("radio").length, 6);
	assert.equal((screen.getByRole("radio", { name: "估算" }) as HTMLInputElement).disabled, true);
	assert.ok(screen.getByText(/按 token 长度必须提供可加载/));
});

test("the automatic source asks the target's tokenize API first, on a managed target and an external endpoint alike", () => {
	render(<I18nextProvider i18n={i18n}><Form initialValues={{ tokenizer_source: "auto" }}><TokenizerSourceFields source="auto" dataset="random" targetKind="instance" onUpload={() => {}} /></Form></I18nextProvider>);
	assert.equal((screen.getByRole("radio", { name: "自动（推理服务分词接口 / 目标机 / 模型仓库）" }) as HTMLInputElement).disabled, false);
	assert.equal((screen.getByRole("radio", { name: "估算" }) as HTMLInputElement).disabled, true);
	assert.ok(screen.getByText(/优先用推理服务自己的 \/v1\/tokenize/));
	// Nothing to type: the source is resolved rather than entered.
	assert.equal(screen.queryByLabelText("模型仓库"), null);
	cleanup();

	render(<I18nextProvider i18n={i18n}><Form initialValues={{ tokenizer_source: "auto" }}><TokenizerSourceFields source="auto" dataset="random" targetKind="external" onUpload={() => {}} /></Form></I18nextProvider>);
	assert.equal((screen.getByRole("radio", { name: "自动（推理服务分词接口 / 目标机 / 模型仓库）" }) as HTMLInputElement).disabled, false);
	assert.ok(screen.getByText(/外部端点使用端点自己的/));
});

test("the inference service's tokenize API is a source of its own that falls back to nothing", async () => {
	render(<I18nextProvider i18n={i18n}><Form initialValues={{ tokenizer_source: "engine" }}><TokenizerSourceFields source="engine" dataset="agentic" targetKind="external" onUpload={() => {}} /></Form></I18nextProvider>);
	const engine = screen.getByRole("radio", { name: "推理服务 (de)tokenize 接口" }) as HTMLInputElement;
	assert.equal(engine.checked, true);
	assert.ok(screen.getByText(/不会改用其他来源/));
	assert.equal(screen.queryByLabelText("模型仓库"), null);
	assert.equal(screen.queryByRole("button", { name: /上传/ }), null);
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

const userEvent = (await import("@testing-library/user-event")).default;

for (const targetKind of ["instance", "external"] as const) {
	for (const source of ["huggingface", "modelscope"] as const) {
		test(`${targetKind} ${source} requires a nonblank repository before submission`, async () => {
			const sent: string[] = [];
			render(<I18nextProvider i18n={i18n}><Form initialValues={{ tokenizer_source: source }} onFinish={(values) => sent.push(tokenizerReference(source, values.tokenizer_repo, undefined, { huggingface: "hf", modelscope: "ms" })!)}>
				<TokenizerSourceFields source={source} dataset="random" targetKind={targetKind} onUpload={() => {}} />
				<button type="submit">开始评测</button>
			</Form></I18nextProvider>);
			const user = userEvent.setup({ document: dom.window.document });
			await user.click(screen.getByRole("button", { name: "开始评测" }));
			assert.ok(await screen.findByText("请输入模型仓库"));
			assert.deepEqual(sent, []);
			const repository = screen.getByRole("textbox", { name: "模型仓库" });
			await user.type(repository, "   ");
			await user.click(screen.getByRole("button", { name: "开始评测" }));
			assert.ok(await screen.findByText("请输入模型仓库"));
			assert.deepEqual(sent, []);
			await user.clear(repository);
			await user.type(repository, " Qwen/Qwen3-8B ");
			await user.click(screen.getByRole("button", { name: "开始评测" }));
			await waitFor(() => assert.deepEqual(sent, [`${source === "huggingface" ? "hf" : "ms"}://Qwen/Qwen3-8B`]));
		});
	}
	for (const dataset of ["random", "agentic", "multiturn"]) {
		test(`${targetKind} ${dataset} upload requires a file and shows a visible validation error`, async () => {
			const sent: string[] = [];
			function Harness() {
				const [form] = Form.useForm();
				const reference = Form.useWatch("tokenizer_ref", form);
				return <Form form={form} initialValues={{ tokenizer_source: "upload", tokenizer_ref: "  " }} onFinish={(values) => sent.push(values.tokenizer_ref)}>
					<TokenizerSourceFields source="upload" dataset={dataset} targetKind={targetKind} tokenizerRef={reference} onUpload={(file) => form.setFieldValue("tokenizer_ref", `/tmp/${file.name}`)} />
					<button type="submit">开始评测</button>
				</Form>;
			}
			const { container } = render(<I18nextProvider i18n={i18n}><Harness /></I18nextProvider>);
			const user = userEvent.setup({ document: dom.window.document });
			await user.click(screen.getByRole("button", { name: "开始评测" }));
			assert.ok(await screen.findByText("请上传 tokenizer.json"));
			assert.deepEqual(sent, []);
			const input = container.querySelector('input[type="file"]') as HTMLInputElement;
			await user.upload(input, new dom.window.File(["{}"], "selected.json", { type: "application/json" }));
			assert.ok(await screen.findByText("当前文件：selected.json"));
			await user.click(screen.getByRole("button", { name: "开始评测" }));
			await waitFor(() => assert.deepEqual(sent, ["/tmp/selected.json"]));
		});
	}
}

test("automatic managed resolution and natural-text estimation need no explicit reference", async () => {
	for (const [targetKind, source] of [["instance", "auto"], ["external", "estimate"]] as const) {
		let submitted = false;
		render(<I18nextProvider i18n={i18n}><Form initialValues={{ tokenizer_source: source }} onFinish={() => { submitted = true; }}>
			<TokenizerSourceFields source={source} dataset="multiturn" targetKind={targetKind} onUpload={() => {}} />
			<button type="submit">开始评测</button>
		</Form></I18nextProvider>);
		await userEvent.setup({ document: dom.window.document }).click(screen.getByRole("button", { name: "开始评测" }));
		await waitFor(() => assert.equal(submitted, true));
		cleanup();
	}
});

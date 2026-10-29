import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import type { PlaygroundCatalog, PlaygroundModel } from "../src/pages/playground/types.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
const nativeBlob = globalThis.Blob;
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "File", "FileReader", "Blob", "FormData", "DOMException"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "sessionStorage", { value: dom.window.sessionStorage, configurable: true });
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
Object.defineProperty(dom.window, "requestAnimationFrame", { configurable: true, value: (callback: FrameRequestCallback) => dom.window.setTimeout(() => callback(performance.now()), 0) });
Object.defineProperty(dom.window, "cancelAnimationFrame", { configurable: true, value: (id: number) => dom.window.clearTimeout(id) });
Object.defineProperty(globalThis, "requestAnimationFrame", { configurable: true, value: dom.window.requestAnimationFrame });
Object.defineProperty(globalThis, "cancelAnimationFrame", { configurable: true, value: dom.window.cancelAnimationFrame });
Object.defineProperty(globalThis.URL, "createObjectURL", { configurable: true, value: () => "blob:playground-result" });
Object.defineProperty(globalThis.URL, "revokeObjectURL", { configurable: true, value: () => undefined });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
class WebSocketStub {
  static readonly OPEN = 1;
  static instances: WebSocketStub[] = [];
  readonly url: string;
  readyState = 0;
  sent: string[] = [];
  onopen?: () => void;
  onmessage?: (event: { data: string }) => void;
  onerror?: () => void;
  onclose?: () => void;
  constructor(url: string) { this.url = url; WebSocketStub.instances.push(this); }
  open() { this.readyState = WebSocketStub.OPEN; this.onopen?.(); }
  send(value: string) { this.sent.push(value); }
  close() { this.readyState = 3; this.onclose?.(); }
}
Object.defineProperty(globalThis, "WebSocket", { value: WebSocketStub, configurable: true });
Object.defineProperty(dom.window, "WebSocket", { value: WebSocketStub, configurable: true });
Object.defineProperties(dom.window.HTMLElement.prototype, {
  attachEvent: { configurable: true, value() {} },
  detachEvent: { configurable: true, value() {} },
  scrollTo: { configurable: true, value() {} },
});

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: {
    playground: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/playground.json", import.meta.url), "utf8")),
    common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")),
  }, zh: {
    playground: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/zh/playground.json", import.meta.url), "utf8")),
    common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/zh/common.json", import.meta.url), "utf8")),
  } },
  defaultNS: "playground",
  interpolation: { escapeValue: false },
});

const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const { act, cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: PlaygroundPage } = await import("../src/pages/playground/PlaygroundPage.tsx");
const { default: ChatPage } = await import("../src/pages/playground/ChatPage.tsx");
const { ConversationStore } = await import("../src/pages/playground/conversations/store.ts");
const { ConversationStoreContext } = await import("../src/pages/playground/conversations/useConversations.ts");
const { default: AudioPage } = await import("../src/pages/playground/AudioPage.tsx");
const { default: VectorPage } = await import("../src/pages/playground/VectorPage.tsx");
const { default: CompletionPage } = await import("../src/pages/playground/CompletionPage.tsx");
const { default: ImagePage } = await import("../src/pages/playground/ImagePage.tsx");
const { default: VideoPage } = await import("../src/pages/playground/VideoPage.tsx");
const { default: RealtimePage } = await import("../src/pages/playground/RealtimePage.tsx");
const originalFetch = globalThis.fetch;

const asyncVideo = {
  id_field: "id", status_field: "status", success_statuses: ["completed"], failure_statuses: ["failed", "cancelled", "deleted"],
  poll_method: "GET", poll_endpoint: "{endpoint}/{id}", content_method: "GET", content_endpoint: "{endpoint}/{id}/content", cancel_method: "DELETE", cancel_endpoint: "{endpoint}/{id}",
};
const model = (id: string, operations: PlaygroundModel["operations"]): PlaygroundModel => ({
  id, label: id, capability: "chat", operations, sleeping: false, public_data_plane: true,
});
const operation = (type: PlaygroundModel["operations"][number]["type"], endpoint: string, renderer: PlaygroundModel["operations"][number]["renderer"], response: PlaygroundModel["operations"][number]["response"], mode: PlaygroundModel["operations"][number]["mode"] = "sync") => ({
  type, endpoint, renderer, response, mode, request_encodings: ["json" as const],
});
const catalog: PlaygroundCatalog = {
  models: [
    model("chat-model", [operation("openai.chat", "/v1/chat/completions", "chat", "sse", "stream")]),
    model("vector-model", [operation("openai.embeddings", "/v1/embeddings", "vector", "json"), operation("openai.rerank", "/v1/rerank", "rerank", "json")]),
    model("audio-model", [operation("openai.audio.transcribe", "/v1/audio/transcriptions", "audio", "json"), operation("openai.audio.speech", "/v1/audio/speech", "audio", "audio")]),
    model("image-model", [operation("openai.image.generate", "/v1/images/generations", "image", "image")]),
    model("video-model", [{ ...operation("openai.video.generate", "/v1/videos", "video", "video", "async"), async: asyncVideo }]),
    model("realtime-model", [{ ...operation("openai.realtime", "/v1/realtime", "realtime", "websocket", "realtime"), request_encodings: ["websocket"] }]),
    model("translate-model", [{ ...operation("openai.completions", "/v1/completions", "completion", "sse", "stream"), request_defaults: { stop: ["<|plamo:op|>"], temperature: 0 } }]),
  ],
  parameters: [],
  html_preview: { allow_network: false },
};

afterEach(() => {
  cleanup();
  dom.window.sessionStorage.clear();
  globalThis.fetch = originalFetch;
  WebSocketStub.instances = [];
});

test("Realtime playground opens the declared WebSocket route and sends validated JSON events", async () => {
  renderAt(<RealtimePage catalog={catalog} />, "/playground/realtime");
  await userEvent.setup().click(screen.getByRole("button", { name: "Connect" }));
  const socket = WebSocketStub.instances[0];
  assert.equal(socket.url, "ws://localhost/api/v1/playground/v1/realtime?model=realtime-model");
  socket.open();
  assert.equal((await screen.findAllByText("Connected")).length, 2);
  await userEvent.setup().click(screen.getByRole("button", { name: "Send Event" }));
    assert.deepEqual(socket.sent, ['{"type":"session.update","model":"realtime-model","session":{}}']);
  socket.onmessage?.({ data: '{"type":"session.created"}' });
  assert.ok(await screen.findByText(/session\.created/));
});

test("Playground catalog renders loading, error, retry, and operation-specific empty states", async () => {
  let calls = 0;
  globalThis.fetch = (async () => {
    calls++;
    if (calls === 1) return jsonResponse({ error: { message: "catalog offline" } }, 503);
    return jsonResponse({ ...catalog, models: [] });
  }) as typeof fetch;
  renderAt(<PlaygroundPage />, "/playground/image");

  assert.ok(document.querySelector(".ant-skeleton"));
  assert.equal((await screen.findByText("catalog offline")).textContent, "catalog offline");
  const retry = screen.getByRole("button", { name: "Retry" });
  retry.focus();
  await userEvent.setup().keyboard("{Enter}");
  assert.equal((await screen.findByText("No deployed model declares image generation")).textContent, "No deployed model declares image generation");
  assert.equal(calls, 2);
});

test("Chat sends an SSE request selected from the declared operation", async () => {
  const requests: Array<{ url: string; body: Record<string, unknown> }> = [];
  globalThis.fetch = (async (input, init) => {
    const user = me(input);
    if (user) return user;
    requests.push({ url: String(input), body: JSON.parse(String(init?.body)) as Record<string, unknown> });
    return new Response("data: {\"choices\":[{\"delta\":{\"content\":\"hello from SSE\"}}]}\n\ndata: [DONE]\n\n", { headers: { "Content-Type": "text/event-stream" } });
  }) as typeof fetch;
  renderAt(<ChatPage catalog={catalog} onCatalogChanged={() => undefined} />, "/playground/chat");

  await userEvent.setup().type(await screen.findByRole("textbox", { name: "Message Input" }), "hello");
  await userEvent.setup().click(screen.getByRole("button", { name: /Send/ }));
  assert.ok(await screen.findByText("hello from SSE"));
  assert.equal(requests[0].url, "/api/v1/playground/v1/chat/completions");
  assert.equal(requests[0].body.model, "chat-model");
  assert.equal(requests[0].body.stream, true);
  assert.deepEqual(requests[0].body.messages, [{ role: "user", content: "hello" }]);
});

test("Vector page intersects models with embedding and rerank operations and sends their exact fields", async () => {
  const requests: Array<{ url: string; body: Record<string, unknown> }> = [];
  globalThis.fetch = (async (input, init) => {
    const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
    requests.push({ url: String(input), body });
    return jsonResponse(String(input).endsWith("/v1/rerank") ? { results: [{ index: 0, relevance_score: 1 }] } : { data: [{ embedding: [0.1, 0.2] }] });
  }) as typeof fetch;
  renderAt(<VectorPage catalog={catalog} />, "/playground/vector");

	await userEvent.setup().click(screen.getByRole("button", { name: "Run" }));
	assert.ok(await screen.findByText(/0\.1/));
	await userEvent.setup().click(screen.getByText("Rerank"));
	await userEvent.setup().click(screen.getByText("Run").closest("button") as HTMLButtonElement);
  assert.ok(await screen.findByText(/relevance_score/));
  assert.equal(requests[0].url, "/api/v1/playground/v1/embeddings");
  assert.deepEqual(requests[0].body, { model: "vector-model", input: "GPU inference" });
  assert.equal(requests[1].url, "/api/v1/playground/v1/rerank");
  assert.equal(requests[1].body.model, "vector-model");
  assert.equal(requests[1].body.query, "GPU inference");
  assert.deepEqual(requests[1].body.documents, ["GPU inference runs on accelerators.", "This document is unrelated."]);
});

test("Audio speech, image generation, and async video render binary or visual results", async () => {
  const calls: Array<{ url: string; method: string; body?: Record<string, unknown> }> = [];
  globalThis.fetch = (async (input, init) => {
    const url = String(input);
    const method = init?.method || "GET";
    const body = typeof init?.body === "string" ? JSON.parse(init.body) as Record<string, unknown> : undefined;
    calls.push({ url, method, body });
    if (url.endsWith("/v1/audio/speech")) return new Response(new nativeBlob(["RIFF-audio"]), { headers: { "Content-Type": "audio/wav" } });
    if (url.endsWith("/v1/images/generations")) return jsonResponse({ data: [{ b64_json: "iVBORw0KGgo=" }] });
    if (url.endsWith("/v1/videos") && method === "POST") return jsonResponse({ id: "video-1", status: "queued" }, 202);
    if (url.includes("/v1/videos/video-1/content")) return new Response(new nativeBlob(["video"]), { headers: { "Content-Type": "video/mp4" } });
    if (url.includes("/v1/videos/video-1")) return jsonResponse({ id: "video-1", status: "completed" });
    return jsonResponse({ error: { message: "unexpected request" } }, 500);
  }) as typeof fetch;

  const audio = renderAt(<AudioPage catalog={catalog} />, "/playground/audio?operation=openai.audio.speech");
  await userEvent.setup().click(screen.getByRole("button", { name: "Generate Speech" }));
  await waitFor(() => assert.ok(audio.container.querySelector('audio[src="blob:playground-result"]')));
  audio.unmount();

  const image = renderAt(<ImagePage catalog={catalog} />, "/playground/image");
  await userEvent.setup().click(screen.getByRole("button", { name: "Generate Image" }));
  await waitFor(() => assert.ok(image.container.querySelector('img[src^="data:image/png;base64"]')));
  image.unmount();

  const video = renderAt(<VideoPage catalog={catalog} />, "/playground/video");
  await userEvent.setup().click(screen.getByRole("button", { name: "Generate Video" }));
  await waitFor(() => assert.ok(video.container.querySelector('video[src="blob:playground-result"]')));
  assert.deepEqual(calls.map((call) => [call.method, call.url]), [
    ["POST", "/api/v1/playground/v1/audio/speech"],
    ["POST", "/api/v1/playground/v1/images/generations"],
    ["POST", "/api/v1/playground/v1/videos"],
    ["GET", "/api/v1/playground/v1/videos/video-1?model=video-model"],
    ["GET", "/api/v1/playground/v1/videos/video-1/content?model=video-model"],
  ]);
  assert.deepEqual(calls[0].body, { model: "audio-model", input: "Hello from GPUWorks" });
  assert.deepEqual(calls[1].body, { model: "image-model", prompt: "A sunrise over mountains", size: "1024x1024", response_format: "b64_json" });
  assert.deepEqual(calls[2].body, { model: "video-model", prompt: "A sunrise over mountains", seconds: 4 });
});

test("Async video exposes cancellation while the first status poll is pending", async () => {
  const calls: Array<[string, string]> = [];
  globalThis.fetch = (async (input, init) => {
    const url = String(input);
    const method = init?.method || "GET";
    calls.push([method, url]);
    if (url.endsWith("/v1/videos") && method === "POST") return jsonResponse({ id: "video-slow", status: "queued" }, 202);
    if (url.includes("/v1/videos/video-slow") && method === "DELETE") return jsonResponse({ id: "video-slow", status: "deleted" });
    if (url.includes("/v1/videos/video-slow") && method === "GET") {
      return new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener("abort", () => reject(new DOMException("Aborted", "AbortError")), { once: true });
      });
    }
    return jsonResponse({ error: { message: "unexpected request" } }, 500);
  }) as typeof fetch;

  renderAt(<VideoPage catalog={catalog} />, "/playground/video");
  await userEvent.setup().click(screen.getByRole("button", { name: "Generate Video" }));
  await userEvent.setup().click(await screen.findByRole("button", { name: "Cancel Job" }));
  assert.ok(await screen.findByText("deleted"));
  assert.deepEqual(calls, [
    ["POST", "/api/v1/playground/v1/videos"],
    ["GET", "/api/v1/playground/v1/videos/video-slow?model=video-model"],
    ["DELETE", "/api/v1/playground/v1/videos/video-slow?model=video-model"],
  ]);
});

test("A completed video does not abort the next generation request", async () => {
  let creates = 0;
  const calls: Array<[string, string]> = [];
  globalThis.fetch = (async (input, init) => {
    const url = String(input);
    const method = init?.method || "GET";
    calls.push([method, url]);
    if (url.endsWith("/v1/videos") && method === "POST") {
      creates++;
      return jsonResponse({ id: creates === 1 ? "video-complete" : "video-next", status: "queued" }, 202);
    }
    if (url.includes("video-complete/content")) return new Response(new nativeBlob(["video"]), { headers: { "Content-Type": "video/mp4" } });
    if (url.includes("video-complete")) return jsonResponse({ id: "video-complete", status: "completed" });
    if (url.includes("video-next") && method === "DELETE") return jsonResponse({ id: "video-next", status: "deleted" });
    if (url.includes("video-next") && method === "GET") {
      return new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener("abort", () => reject(new DOMException("Aborted", "AbortError")), { once: true });
      });
    }
    return jsonResponse({ error: { message: "unexpected request" } }, 500);
  }) as typeof fetch;

  const view = renderAt(<VideoPage catalog={catalog} />, "/playground/video");
  await userEvent.setup().click(screen.getByRole("button", { name: "Generate Video" }));
  await waitFor(() => assert.ok(view.container.querySelector('video[src="blob:playground-result"]')));
  await userEvent.setup().click(screen.getByRole("button", { name: /Generate Video/ }));
  await userEvent.setup().click(await screen.findByRole("button", { name: "Cancel Job" }));
  assert.ok(await screen.findByText("deleted"));
  assert.ok(calls.some(([method, url]) => method === "GET" && url.includes("video-next")), JSON.stringify(calls));
  assert.ok(calls.some(([method, url]) => method === "DELETE" && url.includes("video-next")), JSON.stringify(calls));
});

function LocationProbe() { const location = useLocation(); return <output data-testid="location">{location.pathname}{location.search}</output>; }

// Each render gets a page store of its own that keeps conversations in memory.
const stores: InstanceType<typeof ConversationStore>[] = [];
afterEach(() => { for (const store of stores.splice(0)) store.dispose(); });

function renderAt(component: React.ReactElement, path: string) {
  const store = new ConversationStore({ openDB: () => Promise.reject(new Error("no IndexedDB in this test")), peers: () => ({ post() {}, listen() {}, close() {} }), session: dom.window.sessionStorage });
  stores.push(store);
  return render(<ConversationStoreContext.Provider value={store}><I18nextProvider i18n={i18n}><MemoryRouter initialEntries={[path]}><LocationProbe /><Routes><Route path="*" element={component} /></Routes></MemoryRouter></I18nextProvider></ConversationStoreContext.Provider>);
}

const me = (input: RequestInfo | URL) => String(input).endsWith("/api/v1/auth/me") ? jsonResponse({ id: 1, username: "admin", role: "admin" }) : undefined;

function jsonResponse(value: unknown, status = 200): Response {
  return new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });
}


test("Playground never rewrites an unsupported model into another model", async () => {
  let requests = 0;
  globalThis.fetch = (async (input: RequestInfo | URL) => me(input) ?? (requests++, Promise.reject(new Error("Unexpected inference")))) as typeof fetch;
  renderAt(<ChatPage catalog={catalog} onCatalogChanged={() => undefined} />, "/playground/chat?model=audio-model");
  assert.ok(await screen.findByText("This model does not declare this operation: audio-model"));
  assert.equal(screen.getByTestId("location").textContent, "/playground/chat?model=audio-model");
  assert.equal(screen.getByRole("link", { name: "openai.audio.speech" }).getAttribute("href"), "/playground/audio?operation=openai.audio.speech&model=audio-model");
  await userEvent.setup().type(screen.getByRole("textbox", { name: "Message Input" }), "hello");
  await userEvent.setup().click(screen.getByRole("button", { name: /Send/ }));
  assert.equal(requests, 0);
});

test("Every operation page preserves an unsupported model and offers its declared interfaces", async () => {
  const pages = [
    [AudioPage, "audio"], [VectorPage, "vector"], [ImagePage, "image"], [VideoPage, "video"], [RealtimePage, "realtime"],
  ] as const;
  for (const [Page, route] of pages) {
    const view = renderAt(<Page catalog={catalog} />, `/playground/${route}?model=chat-model`);
    assert.ok(await screen.findByText("This model does not declare this operation: chat-model"));
    assert.ok(screen.getByTestId("location").textContent?.includes("model=chat-model"));
    assert.equal(screen.getByRole("link", { name: "openai.chat" }).getAttribute("href"), "/playground/chat?operation=openai.chat&model=chat-model");
    view.unmount();
  }
});

const { useDiscovery } = await import("../src/pages/playground/useDiscovery.ts");
function DiscoveryProbe() {
  const [deployment, setDeployment] = React.useState(101);
  const [sleeping, setSleeping] = React.useState(false);
  const discovery = useDiscovery({ ...catalog.models[2], deployment_id: deployment, sleeping }, { name: "voices", endpoint: "/v1/audio/voices" });
  return <><button onClick={() => setDeployment((value) => value === 101 ? 102 : 101)}>Switch deployment</button><button onClick={() => setSleeping((value) => !value)}>Toggle sleep</button><button onClick={discovery.reload}>Retry discovery</button><output data-testid="discovery">{discovery.loading ? "Loading voices" : discovery.error || JSON.stringify(discovery.data) || "Unavailable"}</output></>;
}

test("Discovery caches successful facts per deployment and refreshes after sleep or failure", async () => {
  let calls = 0;
  let fail = false;
  globalThis.fetch = (async (input, init) => {
    calls++;
    assert.equal(String(input), "/api/v1/playground/v1/audio/voices?model=audio-model");
    assert.equal(init?.credentials, "include");
    return fail ? jsonResponse({ error: "offline" }, 503) : jsonResponse({ voices: [calls === 1 ? "vivian" : "ryan"], uploaded_voices: [] });
  }) as typeof fetch;
  renderAt(<DiscoveryProbe />, "/playground/audio");
  await waitFor(() => assert.match(screen.getByTestId("discovery").textContent || "", /vivian/));
  await userEvent.setup().click(screen.getByRole("button", { name: "Switch deployment" }));
  await waitFor(() => assert.match(screen.getByTestId("discovery").textContent || "", /ryan/));
  await userEvent.setup().click(screen.getByRole("button", { name: "Switch deployment" }));
  await waitFor(() => assert.match(screen.getByTestId("discovery").textContent || "", /vivian/));
  assert.equal(calls, 2);
  await userEvent.setup().click(screen.getByRole("button", { name: "Toggle sleep" }));
  assert.equal(screen.getByTestId("discovery").textContent, "Unavailable");
  assert.equal(calls, 2);
  fail = true;
  await userEvent.setup().click(screen.getByRole("button", { name: "Toggle sleep" }));
  await waitFor(() => assert.match(screen.getByTestId("discovery").textContent || "", /503/));
  assert.equal(calls, 3);
  fail = false;
  await userEvent.setup().click(screen.getByRole("button", { name: "Retry discovery" }));
  await waitFor(() => assert.match(screen.getByTestId("discovery").textContent || "", /ryan/));
  assert.equal(calls, 4);
});

test("Discovery cancels stale requests and never displays another deployment's response", async () => {
  const signals: AbortSignal[] = [];
  let resolveOld: (value: Response) => void = () => undefined;
  globalThis.fetch = (async (_input, init) => {
    signals.push(init?.signal as AbortSignal);
    if (signals.length === 1) return new Promise<Response>((resolve) => { resolveOld = resolve; });
    return jsonResponse({ voices: ["current"] });
  }) as typeof fetch;
  function Probe() {
    const [id, setID] = React.useState("stale-discovery");
    const { data, loading } = useDiscovery({ ...catalog.models[2], id }, { name: "voices", endpoint: "/v1/audio/voices" });
    return <><button onClick={() => setID("current-discovery")}>Change model</button><output data-testid="stale-discovery-result">{loading ? "Loading" : JSON.stringify(data)}</output></>;
  }
  const view = renderAt(<Probe />, "/playground/audio");
  await userEvent.setup().click(screen.getByRole("button", { name: "Change model" }));
  await waitFor(() => assert.match(screen.getByTestId("stale-discovery-result").textContent || "", /current/));
  assert.equal(signals[0].aborted, true);
  resolveOld(jsonResponse({ voices: ["stale"] }));
  await waitFor(() => assert.doesNotMatch(screen.getByTestId("stale-discovery-result").textContent || "", /stale/));
  view.unmount();
  assert.equal(signals[1].aborted, true);
});

// A real vLLM 0.30 text completion stream (Qwen3-0.6B), the same capture the
// gateway's observer is tested on: text in choices[0].text, usage last.
const completionStream = fs.readFileSync(new URL("../../internal/server/proxy/testdata/vllm-0.30-completions-usage.sse", import.meta.url), "utf8");

test("Completion sends the raw prompt with the declared defaults and streams the continuation", async () => {
  const requests: Array<{ url: string; body: Record<string, unknown> }> = [];
  globalThis.fetch = (async (input, init) => {
    requests.push({ url: String(input), body: JSON.parse(String(init?.body)) as Record<string, unknown> });
    return new Response(completionStream, { headers: { "Content-Type": "text/event-stream" } });
  }) as typeof fetch;
  renderAt(<CompletionPage catalog={catalog} />, "/playground/completion");
  const prompt = await screen.findByRole("textbox", { name: "Prompt" });
  await userEvent.setup().clear(prompt);
  await userEvent.setup().type(prompt, "The capital of France is");
  await userEvent.setup().click(screen.getByRole("button", { name: "Generate" }));
  assert.ok(await screen.findByText(/^Paris\. The capital of/));
  assert.ok(await screen.findByText("5 prompt tokens · 16 completion tokens"));
  assert.equal(requests.length, 1);
  assert.equal(requests[0].url, "/api/v1/playground/v1/completions");
  assert.deepEqual(requests[0].body, { model: "translate-model", prompt: "The capital of France is", max_tokens: 256, stop: ["<|plamo:op|>"], temperature: 0, stream: true });
  assert.equal(screen.getByTestId("location").textContent, "/playground/completion?operation=openai.completions&model=translate-model");
});

test("Completion sends on Ctrl+Enter, shows the gateway's refusal, and is empty without a completion model", async () => {
  let calls = 0;
  globalThis.fetch = (async () => {
    calls++;
    return jsonResponse({ error: { message: "deployment translate-model does not expose POST /v1/completions" } }, 404);
  }) as typeof fetch;
  renderAt(<CompletionPage catalog={catalog} />, "/playground/completion");
  const prompt = await screen.findByRole("textbox", { name: "Prompt" });
  prompt.focus();
  await userEvent.setup().keyboard("{Control>}{Enter}{/Control}");
  assert.ok(await screen.findByText("deployment translate-model does not expose POST /v1/completions"));
  assert.equal(calls, 1);
  cleanup();

  renderAt(<CompletionPage catalog={{ ...catalog, models: catalog.models.filter((item) => item.id !== "translate-model") }} />, "/playground/completion");
  assert.ok(await screen.findByText("No deployed model declares text completion"));
  cleanup();

  renderAt(<CompletionPage catalog={catalog} />, "/playground/completion?model=chat-model");
  assert.ok(await screen.findByText("This model does not declare this operation: chat-model"));
});

test("A completion declared as sync is sent without streaming and its text shown", async () => {
  const sync: PlaygroundCatalog = { ...catalog, models: [model("base-model", [operation("openai.completions", "/v1/completions", "completion", "sse", "sync")])] };
  let body: Record<string, unknown> = {};
  globalThis.fetch = (async (_input, init) => {
    body = JSON.parse(String(init?.body)) as Record<string, unknown>;
    return jsonResponse({ choices: [{ index: 0, text: " Paris." }], usage: { prompt_tokens: 5, completion_tokens: 2 } });
  }) as typeof fetch;
  renderAt(<CompletionPage catalog={sync} />, "/playground/completion");
  await userEvent.setup().click(await screen.findByRole("button", { name: "Generate" }));
  assert.ok(await screen.findByText(/Paris\./));
  assert.equal(body.stream, false);
  assert.ok(screen.getByText("5 prompt tokens · 2 completion tokens"));
});

test("A completion model that declares a working prompt starts with it, so Generate works as is", async () => {
  const tagged = "<|plamo:op|>dataset\ntranslation\n<|plamo:op|>input lang=English\nHello\n<|plamo:op|>output lang=Japanese\n";
  const models: PlaygroundCatalog = { ...catalog, models: [
    model("plamo", [{ ...operation("openai.completions", "/v1/completions", "completion", "sse", "stream"), request_defaults: { stop: ["<|plamo:op|>"], temperature: 0 }, example_input: { prompt: tagged } }]),
    model("base-model", [operation("openai.completions", "/v1/completions", "completion", "sse", "stream")]),
  ] };
  let body: Record<string, unknown> = {};
  globalThis.fetch = (async (_input, init) => {
    body = JSON.parse(String(init?.body)) as Record<string, unknown>;
    return new Response(completionStream, { headers: { "Content-Type": "text/event-stream" } });
  }) as typeof fetch;
  renderAt(<CompletionPage catalog={models} />, "/playground/completion");
  const prompt = await screen.findByRole("textbox", { name: "Prompt" }) as HTMLTextAreaElement;
  assert.equal(prompt.value, tagged);
  await userEvent.setup().click(screen.getByRole("button", { name: "Generate" }));
  assert.ok(await screen.findByText("5 prompt tokens · 16 completion tokens"));
  assert.equal(body.prompt, tagged);
  assert.deepEqual(body.stop, ["<|plamo:op|>"]);
  cleanup();

  // A model without a declared prompt starts from the generic one.
  renderAt(<CompletionPage catalog={models} />, "/playground/completion?model=base-model");
  assert.equal((await screen.findByRole("textbox", { name: "Prompt" }) as HTMLTextAreaElement).value, "The capital of France is");
});

for (const declaresVoice of [false, true]) {
  test(`Audio speech ${declaresVoice ? "preserves a selected declared voice" : "does not inject an undeclared discovered voice"}`, async () => {
    const id = declaresVoice ? "declared-voice-model" : "reference-audio-model";
    const audioCatalog: PlaygroundCatalog = { ...catalog, models: [model(id, [{
      ...operation("openai.audio.speech", "/v1/audio/speech", "audio", "audio"),
      discovery: [{ name: "voices", endpoint: "/v1/audio/voices" }],
      inputs: declaresVoice ? [{ name: "voice", kind: "select", label: "Voice", required: true }] : [{ name: "ref_audio", kind: "audio", encoding: "data_url" }],
    }])] };
    let resolveVoices!: (response: Response) => void;
    const voices = new Promise<Response>((resolve) => { resolveVoices = resolve; });
    const requests: Record<string, unknown>[] = [];
    globalThis.fetch = (async (input, init) => {
      if (String(input).includes("/v1/audio/voices?")) return voices;
      if (String(input).endsWith("/v1/audio/speech") && init?.method === "POST") {
        requests.push(JSON.parse(String(init.body)) as Record<string, unknown>);
        return new Response(new nativeBlob(["RIFF-audio"]), { headers: { "Content-Type": "audio/wav" } });
      }
      return jsonResponse({ error: "unexpected request" }, 500);
    }) as typeof fetch;
    const view = renderAt(<AudioPage catalog={audioCatalog} />, `/playground/audio?operation=openai.audio.speech&model=${id}`);
    await act(async () => { resolveVoices(jsonResponse({ voices: ["default", "ryan"], uploaded_voices: [] })); });
    const user = userEvent.setup();
    if (declaresVoice) {
      assert.ok(screen.getByText("default", { selector: ".ant-select-selection-item" }));
      await user.click(await screen.findByRole("combobox", { name: "Voice" }));
      await user.click(screen.getByText("ryan", { selector: ".ant-select-item-option-content" }));
    }
    await user.click(screen.getByRole("button", { name: "Generate Speech" }));
    await waitFor(() => assert.ok(view.container.querySelector('audio[src="blob:playground-result"]')));
    assert.deepEqual(requests, [{ model: id, input: "Hello from GPUWorks", ...(declaresVoice ? { voice: "ryan" } : {}) }]);
  });
}

test("Switching to a reference-audio model drops the previous model's voice", async () => {
  const audioCatalog: PlaygroundCatalog = { ...catalog, models: [
    model("switch-voice-model", [{ ...operation("openai.audio.speech", "/v1/audio/speech", "audio", "audio"), discovery: [{ name: "voices", endpoint: "/v1/audio/voices" }], inputs: [{ name: "voice", kind: "select", label: "Voice", required: true }] }]),
    model("switch-reference-model", [{ ...operation("openai.audio.speech", "/v1/audio/speech", "audio", "audio"), discovery: [{ name: "voices", endpoint: "/v1/audio/voices" }], inputs: [{ name: "ref_audio", kind: "audio", encoding: "data_url" }] }]),
  ] };
  const requests: Record<string, unknown>[] = [];
  globalThis.fetch = (async (input, init) => {
    if (String(input).includes("/v1/audio/voices?")) return jsonResponse({ voices: ["vivian"], uploaded_voices: [] });
    if (String(input).endsWith("/v1/audio/speech") && init?.method === "POST") {
      requests.push(JSON.parse(String(init.body)) as Record<string, unknown>);
      return new Response(new nativeBlob(["RIFF-audio"]), { headers: { "Content-Type": "audio/wav" } });
    }
    return jsonResponse({ error: "unexpected request" }, 500);
  }) as typeof fetch;
  const view = renderAt(<AudioPage catalog={audioCatalog} />, "/playground/audio?operation=openai.audio.speech&model=switch-voice-model");
  await screen.findByRole("combobox", { name: "Voice" });
  const user = userEvent.setup();
  await user.click(screen.getAllByRole("combobox").find((element) => !element.getAttribute("aria-label"))!);
  await user.click(await screen.findByText("switch-reference-model", { selector: ".ant-select-item-option-content" }));
  await waitFor(() => assert.ok(screen.getByTestId("location").textContent?.includes("model=switch-reference-model")));
  await user.click(screen.getByRole("button", { name: "Request", exact: true }));
  const draft = JSON.parse((screen.getByRole("textbox", { name: "Raw request body" }) as HTMLTextAreaElement).value);
  assert.deepEqual(draft, { model: "switch-reference-model", input: "Hello from GPUWorks" });
  await user.click(screen.getByRole("button", { name: "Close", exact: true }));
  await user.click(screen.getByRole("button", { name: "Generate Speech" }));
  await waitFor(() => assert.ok(view.container.querySelector('audio[src="blob:playground-result"]')));
  assert.deepEqual(requests, [{ model: "switch-reference-model", input: "Hello from GPUWorks" }]);
});

test("Reference audio uses shared translations without a template label and keeps its API input name", async () => {
  const previousLanguage = i18n.language;
  await act(async () => { await i18n.changeLanguage("zh"); });
  try {
    const audioCatalog: PlaygroundCatalog = { ...catalog, models: [model("localized-reference-model", [{
      ...operation("openai.audio.speech", "/v1/audio/speech", "audio", "audio"),
      inputs: [{ name: "ref_audio", kind: "audio", encoding: "data_url" }],
    }])] };
    const requests: Record<string, unknown>[] = [];
    globalThis.fetch = (async (input, init) => {
      if (String(input).endsWith("/v1/audio/speech") && init?.method === "POST") {
        requests.push(JSON.parse(String(init.body)) as Record<string, unknown>);
        return new Response(new nativeBlob(["RIFF-audio"]), { headers: { "Content-Type": "audio/wav" } });
      }
      return jsonResponse({ error: "unexpected request" }, 500);
    }) as typeof fetch;
    const view = renderAt(<AudioPage catalog={audioCatalog} />, "/playground/audio?operation=openai.audio.speech&model=localized-reference-model");
    const user = userEvent.setup();
    await user.upload(screen.getByLabelText("参考音频"), new File(["reference audio"], "reference.wav", { type: "audio/wav" }));
    await user.click(screen.getByRole("button", { name: "Request", exact: true }));
    await waitFor(() => {
      const draft = JSON.parse((screen.getByRole("textbox", { name: "Raw request body" }) as HTMLTextAreaElement).value);
      assert.match(String(draft.ref_audio), /^data:audio\/wav;base64,/);
    });
    await user.click(screen.getByRole("button", { name: "Close", exact: true }));
    await user.click(screen.getByRole("button", { name: "生成语音" }));
    await waitFor(() => assert.ok(view.container.querySelector('audio[src="blob:playground-result"]')));
    assert.equal(requests.length, 1);
    assert.deepEqual(Object.keys(requests[0]).sort(), ["input", "model", "ref_audio"]);
    assert.match(String(requests[0].ref_audio), /^data:audio\/wav;base64,/);
    await act(async () => { await i18n.changeLanguage("en"); });
    assert.ok(screen.getByLabelText("Reference audio"));
  } finally {
    await act(async () => { await i18n.changeLanguage(previousLanguage); });
  }
});

test("Template labels take precedence over shared names and unknown inputs keep their API names", async () => {
  const { default: InputsForm } = await import("../src/pages/playground/InputsForm.tsx");
  const previousLanguage = i18n.language;
  await act(async () => { await i18n.changeLanguage("zh"); });
  try {
    render(<I18nextProvider i18n={i18n}><InputsForm fields={[
      { name: "ref_audio", kind: "audio", encoding: "data_url", label: { zh: "音色样本", en: "Voice sample" } },
      { name: "custom_note", kind: "text" },
    ]} values={{}} onChange={() => undefined} /></I18nextProvider>);
    assert.ok(screen.getByLabelText("音色样本"));
    assert.equal(screen.queryByLabelText("参考音频"), null);
    assert.ok(screen.getByLabelText("custom_note"));
    await act(async () => { await i18n.changeLanguage("en"); });
    assert.ok(screen.getByLabelText("Voice sample"));
  } finally {
    await act(async () => { await i18n.changeLanguage(previousLanguage); });
  }
});

import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import type { PlaygroundCatalog } from "../src/pages/playground/types.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://console.example.test/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "File", "Blob", "FormData", "DOMException"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
Object.defineProperties(dom.window.HTMLElement.prototype, {
  attachEvent: { configurable: true, value() {} },
  detachEvent: { configurable: true, value() {} },
});
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "en", resources: { en: { playground: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/playground.json", import.meta.url), "utf8")) } }, defaultNS: "playground", interpolation: { escapeValue: false } });
const { MemoryRouter } = await import("react-router-dom");
const { act, cleanup, render, screen } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: AudioPage } = await import("../src/pages/playground/AudioPage.tsx");
const catalog: PlaygroundCatalog = { models: [{ id: "audio-model", label: "audio-model", capability: "chat", sleeping: false, operations: [
  { type: "openai.audio.transcribe", endpoint: "/v1/audio/transcriptions", renderer: "audio", response: "json", mode: "sync", request_encodings: ["multipart"] },
  { type: "openai.audio.speech", endpoint: "/v1/audio/speech", renderer: "audio", response: "audio", mode: "sync", request_encodings: ["json"] },
] }], parameters: [], html_preview: { allow_network: false } };
const originalFetch = globalThis.fetch;
let stops = 0;
let permissionRequests = 0;
const stream = { getTracks: () => [{ stop: () => { stops++; } }] };
class Recorder {
  static failConstructor = false;
  static failStart = false;
  static instances: Recorder[] = [];
  mimeType = "audio/webm";
  state = "inactive";
  ondataavailable?: (event: { data: Blob }) => void;
  onstop?: (() => void) | null;
  constructor(_stream: MediaStream) {
    if (Recorder.failConstructor) throw new Error("recorder constructor failed");
    Recorder.instances.push(this);
  }
  start() {
    if (Recorder.failStart) throw new Error("recorder start failed");
    this.state = "recording";
  }
  stop() {
    this.state = "inactive";
    this.ondataavailable?.({ data: new Blob(["recorded-audio"], { type: this.mimeType }) });
    this.onstop?.();
  }
}
function browser(secure: boolean, media: unknown = { getUserMedia: async () => { permissionRequests++; return stream; } }, recorder: unknown = Recorder) {
  dom.reconfigure({ url: secure ? "https://console.example.test/" : "http://console.example.test/" });
  Object.defineProperty(dom.window, "isSecureContext", { configurable: true, value: secure });
  Object.defineProperty(navigator, "mediaDevices", { configurable: true, value: media });
  Object.defineProperty(globalThis, "MediaRecorder", { configurable: true, value: recorder });
}
function mount(operation = "openai.audio.transcribe") {
  return render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={[`/playground/audio?operation=${operation}`]}><AudioPage catalog={catalog} /></MemoryRouter></I18nextProvider>);
}
afterEach(() => {
  cleanup();
  globalThis.fetch = originalFetch;
  stops = 0; permissionRequests = 0;
  Recorder.instances = []; Recorder.failConstructor = false; Recorder.failStart = false;
});

test("HTTP recording is disabled with an HTTPS remedy before requesting microphone permission", async () => {
  browser(false);
  mount();
  assert.ok(screen.getByText(/Microphone recording requires HTTPS or localhost/));
  assert.equal((screen.getByRole("button", { name: /Start Recording/ }) as HTMLButtonElement).disabled, true);
  assert.equal((screen.getByRole("button", { name: /Choose Audio/ }) as HTMLButtonElement).disabled, false);
  assert.equal(permissionRequests, 0);
});

test("secure contexts distinguish missing recording APIs from an insecure origin", () => {
  for (const [media, recorder] of [[null, Recorder], [{ getUserMedia: async () => stream }, null]]) {
    browser(true, media, recorder);
    const view = mount();
    assert.ok(screen.getByText(/This browser does not support microphone recording/));
    assert.equal((screen.getByRole("button", { name: /Start Recording/ }) as HTMLButtonElement).disabled, true);
    assert.equal(permissionRequests, 0);
    view.unmount();
  }
});

test("secure recording requests audio only after a click and creates a file on stop", async () => {
  browser(true);
  mount();
  assert.equal(permissionRequests, 0);
  screen.getByRole("button", { name: /Start Recording/ }).focus();
  await userEvent.setup().keyboard("{Enter}");
  assert.equal(permissionRequests, 1);
  assert.equal(Recorder.instances[0].state, "recording");
  await userEvent.setup().click(screen.getByRole("button", { name: /Stop Recording/ }));
  assert.ok(await screen.findByText(/recording-.*\.webm/));
  assert.equal(stops, 1);
  assert.equal((screen.getByRole("button", { name: "Start Transcription" }) as HTMLButtonElement).disabled, false);
});

test("recording failures release captured audio tracks and leave retry available", async () => {
  for (const failure of ["constructor", "start"]) {
    browser(true);
    Recorder.failConstructor = failure === "constructor";
    Recorder.failStart = failure === "start";
    const view = mount();
    const before = stops;
    await userEvent.setup().click(screen.getByRole("button", { name: /Start Recording/ }));
    await screen.findByText(new RegExp(`Unable to start recording: recorder ${failure} failed`));
    assert.equal(stops, before + 1);
    assert.equal(screen.queryByText(/recording-.*\.webm/), null);
    assert.equal((screen.getByRole("button", { name: /Start Recording/ }) as HTMLButtonElement).disabled, false);
    view.unmount();
  }
});

test("permission rejection does not start a recorder", async () => {
  browser(true, { getUserMedia: async () => { throw new DOMException("Permission denied", "NotAllowedError"); } });
  mount();
  await userEvent.setup().click(screen.getByRole("button", { name: /Start Recording/ }));
  assert.ok(await screen.findByText(/Unable to start recording: Permission denied/));
  assert.equal(Recorder.instances.length, 0);
  assert.equal(stops, 0);
});

test("unmount stops recording and late microphone grants are released", async () => {
  browser(true);
  let view = mount();
  await userEvent.setup().click(screen.getByRole("button", { name: /Start Recording/ }));
  view.unmount();
  assert.equal(stops, 1);
  assert.equal(Recorder.instances[0].state, "inactive");
  let grant!: (value: typeof stream) => void;
  browser(true, { getUserMedia: () => new Promise((resolve) => { grant = resolve; }) });
  view = mount();
  await userEvent.setup().click(screen.getByRole("button", { name: /Start Recording/ }));
  view.unmount();
  await act(async () => { grant(stream); });
  assert.equal(stops, 2);
  assert.equal(Recorder.instances.length, 1);
});

test("insecure recording still permits audio upload and sends the real multipart contract", async () => {
  browser(false);
  let request: { url: string; body: FormData } | undefined;
  globalThis.fetch = (async (input, init) => {
    request = { url: String(input), body: init?.body as FormData };
    return new Response(JSON.stringify({ text: "uploaded transcript" }), { headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  const view = mount();
  const input = view.container.querySelector('input[type="file"]') as HTMLInputElement;
  const file = new File(["RIFF-uploaded"], "uploaded.wav", { type: "audio/wav" });
  await userEvent.setup().upload(input, file);
  await userEvent.setup().click(screen.getByRole("button", { name: "Start Transcription" }));
  assert.ok(await screen.findByText("uploaded transcript"));
  assert.equal(request?.url, "/api/v1/playground/v1/audio/transcriptions");
  assert.equal(request?.body.get("model"), "audio-model");
  assert.equal((request?.body.get("file") as File).name, "uploaded.wav");
  assert.equal((request?.body.get("file") as File).size, file.size);
  assert.equal(permissionRequests, 0);
});

test("speech synthesis does not display a microphone security warning", () => {
  browser(false);
  mount("openai.audio.speech");
  assert.equal(screen.queryByRole("button", { name: /Start Recording/ }), null);
  assert.equal(screen.queryByText(/Microphone recording requires HTTPS/), null);
  assert.equal((screen.getByRole("button", { name: "Generate Speech" }) as HTMLButtonElement).disabled, false);
});


test("HTTP localhost remains eligible when the browser identifies a secure context", () => {
  browser(true);
  dom.reconfigure({ url: "http://localhost:8800/" });
  mount();
  assert.equal((screen.getByRole("button", { name: /Start Recording/ }) as HTMLButtonElement).disabled, false);
  assert.equal(screen.queryByText(/Microphone recording requires HTTPS/), null);
  assert.equal(permissionRequests, 0);
});

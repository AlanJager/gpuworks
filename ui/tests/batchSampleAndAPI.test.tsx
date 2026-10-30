import assert from "node:assert/strict";
import { after, afterEach, beforeEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { batch, page } from "./helpers/batchFixtures.ts";

// The sample requests file (downloaded or used as is), the .jsonl-only file
// picker, and the panel that tells the reader batches are an API.

const closeChannels = trackMessageChannels();
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
class Events { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });
const { i18n, locale } = await createI18n(["batches"]);
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { ConfigProvider } = await import("antd");
const { MemoryRouter, Route, Routes } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { batchApi } = await import("../src/batchApi.ts");
const { default: BatchesPage } = await import("../src/pages/BatchesPage.tsx");
const { batchSampleJSONL, sampleFileName, SAMPLE_LINES } = await import("../src/pages/batches/batchSample.ts");
const { batchAPISteps } = await import("../src/pages/batches/BatchAPIUsage.tsx");

const b = locale.batches;
const original = { list: batchApi.list, create: batchApi.create, me: api.me, deployments: api.deployments };

beforeEach(() => {
  localStorage.clear();
  api.me = (async () => ({ role: "admin" })) as never;
  batchApi.list = (async () => page([])) as never;
  api.deployments = (async () => ({
    items: [
      { id: 1, served_name: "qwen-chat", desired_state: "active", runtime_state: "running", state: "running" },
      { id: 2, served_name: "stopped-one", desired_state: "stopped", runtime_state: "stopped", state: "stopped" },
      { id: 3, served_name: "qwen-chat", desired_state: "active", runtime_state: "running", state: "running" },
      { id: 4, served_name: "embedder", desired_state: "active", runtime_state: "running", state: "running" },
    ],
    total: 4,
  })) as never;
});
afterEach(() => {
  cleanup();
  Object.assign(batchApi, { list: original.list, create: original.create });
  api.me = original.me;
  api.deployments = original.deployments;
});
after(() => closeChannels());

function mountPage() {
  render(
    <I18nextProvider i18n={i18n}>
      <ConfigProvider theme={{ token: { motion: false } }}>
        <MemoryRouter initialEntries={["/batches"]}>
          <Routes><Route path="/batches" element={<BatchesPage />} /></Routes>
        </MemoryRouter>
      </ConfigProvider>
    </I18nextProvider>,
  );
  return userEvent.setup({ document: dom.window.document });
}

async function openCreate(user: ReturnType<typeof userEvent.setup>) {
  const create = () => screen.getAllByRole("button", { name: b.action.create })[0] as HTMLButtonElement;
  await waitFor(() => assert.equal(create().disabled, false));
  await user.click(create());
  return screen.findByRole("dialog", { name: b.create.title });
}

test("a sample is valid batch input for every endpoint", () => {
  for (const endpoint of ["/v1/chat/completions", "/v1/completions", "/v1/embeddings"] as const) {
    const lines = batchSampleJSONL(endpoint, "m").trimEnd().split("\n").map((line) => JSON.parse(line));
    assert.equal(lines.length, SAMPLE_LINES);
    assert.equal(new Set(lines.map((line) => line.custom_id)).size, SAMPLE_LINES, "custom_id is unique");
    for (const line of lines) {
      assert.equal(line.method, "POST");
      assert.equal(line.url, endpoint);
      assert.equal(line.body.model, "m");
      assert.equal(line.body.stream, undefined, "no stream: batches refuse streaming requests");
    }
    assert.ok(endpoint !== "/v1/chat/completions" || Array.isArray(lines[0].body.messages));
    assert.ok(endpoint !== "/v1/embeddings" || typeof lines[0].body.input === "string");
    assert.match(sampleFileName(endpoint), /^batch-sample-[a-z-]+\.jsonl$/);
  }
});

test("the file picker takes only .jsonl", async () => {
  const user = mountPage();
  const dialog = await openCreate(user);
  const input = dialog.querySelector("input[type=file]") as HTMLInputElement;
  assert.equal(input.accept, ".jsonl");
  assert.ok(dialog.textContent?.includes(b.create.fileHelp));
});

test("use the sample: it names an active deployment and goes up as the request file", async () => {
  let sent: { file: File; endpoint: string } | null = null;
  batchApi.create = (async (file: File, endpoint: string) => {
    sent = { file, endpoint };
    return batch();
  }) as never;
  const user = mountPage();
  const dialog = await openCreate(user);
  // The model list holds the active deployments' served names, once each.
  await waitFor(() => assert.ok(dialog.textContent?.includes("qwen-chat")));
  assert.ok(!dialog.textContent?.includes("stopped-one"));
  const submit = within(dialog.closest(".ant-modal") as HTMLElement).getByRole("button", { name: b.create.submit }) as HTMLButtonElement;
  assert.equal(submit.disabled, true);
  await user.click(screen.getByRole("button", { name: b.create.sample.use }));
  await waitFor(() => assert.equal(submit.disabled, false));
  await user.click(submit);
  await waitFor(() => assert.ok(sent));
  const got = sent as unknown as { file: File; endpoint: string };
  assert.equal(got.endpoint, "/v1/chat/completions");
  assert.equal(got.file.name, "batch-sample-chat-completions.jsonl");
  const text = await got.file.text();
  assert.equal(JSON.parse(text.split("\n")[0]).body.model, "qwen-chat");
});

test("download the sample hands the browser a .jsonl file", async () => {
  const created: Blob[] = [];
  let clicked = "";
  const urlApi = dom.window.URL as unknown as { createObjectURL: (b: Blob) => string; revokeObjectURL: (u: string) => void };
  const before = { create: urlApi.createObjectURL, revoke: urlApi.revokeObjectURL };
  urlApi.createObjectURL = (blob: Blob) => { created.push(blob); return "blob:sample"; };
  urlApi.revokeObjectURL = () => {};
  Object.defineProperty(globalThis, "URL", { value: dom.window.URL, configurable: true });
  const anchorClick = dom.window.HTMLAnchorElement.prototype.click;
  dom.window.HTMLAnchorElement.prototype.click = function (this: HTMLAnchorElement) { clicked = this.download; };
  try {
    const user = mountPage();
    const dialog = await openCreate(user);
    await waitFor(() => assert.ok(dialog.textContent?.includes("qwen-chat")));
    await user.click(screen.getByRole("button", { name: b.create.sample.download }));
    assert.equal(clicked, "batch-sample-chat-completions.jsonl");
    assert.equal(created.length, 1);
    assert.equal((await created[0].text()).trimEnd().split("\n").length, SAMPLE_LINES);
  } finally {
    dom.window.HTMLAnchorElement.prototype.click = anchorClick;
    urlApi.createObjectURL = before.create;
    urlApi.revokeObjectURL = before.revoke;
  }
});

test("the page says batches are an OpenAI-compatible API and shows the calls", async () => {
  const user = mountPage();
  const header = await screen.findByRole("button", { name: new RegExp(b.api.title.replace(/[()/]/g, ".")) });
  await user.click(header);
  await waitFor(() => assert.ok(screen.getByText(b.api.steps.upload)));
  const link = screen.getByRole("link", { name: b.api.keys }) as HTMLAnchorElement;
  assert.equal(link.getAttribute("href"), "/apikeys");
  const steps = batchAPISteps("http://gw.example");
  assert.deepEqual(steps.map((s) => s.key), ["upload", "create", "poll", "download", "sdk"]);
  assert.ok(steps[0].code.includes("http://gw.example/v1/files") && steps[0].code.includes("purpose=batch"));
  assert.ok(steps[1].code.includes("/v1/batches") && steps[1].code.includes('"completion_window":"24h"'));
  assert.ok(steps[4].code.includes('base_url="http://gw.example/v1"'));
});

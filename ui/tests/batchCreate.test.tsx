import assert from "node:assert/strict";
import { after, afterEach, beforeEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { batch, page } from "./helpers/batchFixtures.ts";

// Creating a batch: the file and endpoint choice, the hint block, the upload
// state, an invalid file named by its line, and the keyboard paths.

const closeChannels = trackMessageChannels();
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
class Events { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });
const { i18n, locale } = await createI18n(["batches"]);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { ConfigProvider } = await import("antd");
const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { batchApi } = await import("../src/batchApi.ts");
const { default: BatchesPage } = await import("../src/pages/BatchesPage.tsx");
const { default: BatchCreateModal } = await import("../src/pages/batches/BatchCreateModal.tsx");
const { batchErrorText } = await import("../src/pages/batches/batchErrors.ts");
type Batch = import("../src/batchApi.ts").Batch;

// user-event does not set keyCode, which the dialogs read; the focused element receives the key.
const escape = () => fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape", code: "Escape", keyCode: 27 });

const b = locale.batches;
const c = locale.common;
const original = { list: batchApi.list, get: batchApi.get, create: batchApi.create, me: api.me };

beforeEach(() => {
  localStorage.clear();
  api.me = (async () => ({ role: "admin" })) as never;
});
afterEach(() => {
  cleanup();
  Object.assign(batchApi, original);
  api.me = original.me;
});
after(() => closeChannels());

function Probe() {
  const location = useLocation();
  return <div data-testid="loc">{location.pathname}{location.search}</div>;
}

function mountPage() {
  render(
    <I18nextProvider i18n={i18n}>
      <ConfigProvider theme={{ token: { motion: false } }}>
        <MemoryRouter initialEntries={["/batches"]}>
          <Routes><Route path="/batches" element={<BatchesPage />} /></Routes>
          <Probe />
        </MemoryRouter>
      </ConfigProvider>
    </I18nextProvider>,
  );
  return userEvent.setup({ document: dom.window.document });
}

const jsonl = (name = "reqs.jsonl", size?: number) => {
  const file = new dom.window.File(['{"custom_id":"1"}\n'], name, { type: "application/x-ndjson" });
  if (size !== undefined) Object.defineProperty(file, "size", { value: size });
  return file;
};

async function openCreate(user: ReturnType<typeof userEvent.setup>) {
  await waitFor(() => assert.equal((screen.getByRole("button", { name: b.action.create }) as HTMLButtonElement).disabled, false));
  await user.click(screen.getByRole("button", { name: b.action.create }));
  return screen.findByRole("dialog", { name: b.create.title });
}

// The dialog moves focus into itself shortly after it opens.
const focusIn = (dialog: HTMLElement) => waitFor(() => assert.ok(dialog.closest(".ant-modal-wrap")?.contains(document.activeElement), "focus moves into the dialog"));

const submitButton = (dialog: HTMLElement) => within(dialog).getByRole("button", { name: b.create.submit }) as HTMLButtonElement;

test("creating a batch uploads the chosen file and endpoint, closes the dialog and opens the new batch", async () => {
  const calls: Array<{ name: string; endpoint: string }> = [];
  let created: Batch | undefined;
  batchApi.list = async () => page(created ? [created] : []);
  batchApi.get = async () => created!;
  batchApi.create = async (file, endpoint) => {
    calls.push({ name: file.name, endpoint });
    created = batch({ id: "batch_new", status: "validating", input_filename: file.name, endpoint });
    return created;
  };
  const user = mountPage();
  const dialog = await openCreate(user);
  assert.equal(submitButton(dialog).disabled, true, "nothing to upload yet");
  for (const hint of [b.create.hint.line, b.create.hint.unique, b.create.hint.sameUrl, b.create.hint.noStream, b.create.hint.window]) {
    assert.ok(within(dialog).getByText(hint));
  }
  assert.ok(within(dialog).getByText("JSON Lines, at most 200 MB and 50,000 requests."));
  const fileInput = within(dialog).getByLabelText(b.create.file) as HTMLInputElement;
  assert.equal(fileInput.getAttribute("accept"), ".jsonl,.json,.txt");
  await user.upload(fileInput, jsonl());
  assert.ok(await within(dialog).findByText(/^reqs\.jsonl \(/));
  await user.click(within(dialog).getByRole("combobox", { name: b.create.endpoint }));
  await user.click(await screen.findByText("/v1/embeddings", { selector: ".ant-select-item-option-content" }));
  await user.click(submitButton(dialog));
  await waitFor(() => assert.deepEqual(calls, [{ name: "reqs.jsonl", endpoint: "/v1/embeddings" }]));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog", { name: b.create.title })));
  await waitFor(() => assert.equal(screen.getByTestId("loc").textContent, "/batches?detail=batch_new"));
  assert.ok(await screen.findByText(b.create.created.replace("{{id}}", "batch_new")));
  const drawer = await screen.findByRole("dialog", { name: b.detail.title.replace("{{id}}", "batch_new") });
  await within(drawer).findByText("reqs.jsonl");
});

test("the endpoint defaults to chat completions and offers the three endpoints", async () => {
  batchApi.list = async () => page([]);
  const sent: string[] = [];
  batchApi.create = async (_file, endpoint) => { sent.push(endpoint); return batch({ id: "batch_n" }); };
  batchApi.get = async () => batch({ id: "batch_n" });
  const user = mountPage();
  const dialog = await openCreate(user);
  const select = within(dialog).getByRole("combobox", { name: b.create.endpoint });
  await user.click(select);
  for (const endpoint of ["/v1/chat/completions", "/v1/completions", "/v1/embeddings"]) {
    assert.ok(await screen.findByText(endpoint, { selector: ".ant-select-item-option-content" }), endpoint);
  }
  escape();
  await user.upload(within(dialog).getByLabelText(b.create.file), jsonl());
  await user.click(submitButton(dialog));
  await waitFor(() => assert.deepEqual(sent, ["/v1/chat/completions"]));
});

test("an invalid file names its line, keeps the dialog open and lets the file be replaced", async () => {
  batchApi.list = async () => page([]);
  let attempts = 0;
  batchApi.create = async () => {
    attempts += 1;
    if (attempts === 1) {
      throw new APIError("BATCH_FILE_INVALID", { line: 7, reason: "missing_custom_id", detail: "line 7: custom_id is required" }, "line 7: custom_id is required", 400);
    }
    return batch({ id: "batch_ok", input_filename: "fixed.jsonl" });
  };
  batchApi.get = async () => batch({ id: "batch_ok", input_filename: "fixed.jsonl" });
  const user = mountPage();
  const dialog = await openCreate(user);
  await user.upload(within(dialog).getByLabelText(b.create.file), jsonl("bad.jsonl"));
  await user.click(submitButton(dialog));
  const alert = await within(dialog).findByText(b.errors.fileLine.replace("{{line}}", "7").replace("{{reason}}", b.errors.fileReasons.missing_custom_id));
  assert.ok(alert);
  assert.ok(within(dialog).getByText(b.errors.detail.replace("{{detail}}", "line 7: custom_id is required")));
  assert.ok(screen.getByRole("dialog", { name: b.create.title }), "the dialog stays open");
  await user.upload(within(dialog).getByLabelText(b.create.file), jsonl("fixed.jsonl"));
  assert.ok(!within(dialog).queryByText(/Line 7/), "choosing another file clears the error");
  await user.click(submitButton(dialog));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog", { name: b.create.title })));
  assert.equal(attempts, 2);
});

test("an error about the whole file or the request is worded without a line", async () => {
  const t = i18n.getFixedT("en", "batches");
  const tc = i18n.getFixedT("en", "common");
  const whole = batchErrorText(t, tc, new APIError("BATCH_FILE_INVALID", { line: 0, reason: "empty_file", detail: "the file has no requests" }, "the file has no requests", 400));
  assert.equal(whole.message, b.errors.fileWhole.replace("{{reason}}", b.errors.fileReasons.empty_file));
  assert.equal(whole.detail, "the file has no requests");
  const mismatch = batchErrorText(t, tc, new APIError("BATCH_REQUEST_INVALID", { field: "endpoint", reason: "endpoint_mismatch" }, "endpoint mismatch", 400));
  assert.equal(mismatch.message, b.errors.requestInvalid.replace("{{field}}", "endpoint").replace("{{reason}}", b.errors.requestReasons.endpoint_mismatch));
  const unknown = batchErrorText(t, tc, new APIError("BATCH_FILE_INVALID", { line: 3, reason: "a_new_reason", detail: "line 3: something new" }, "line 3: something new", 400));
  assert.equal(unknown.message, c.apiError.BATCH_FILE_INVALID.replace("{{detail}}", "line 3: something new"), "an unknown reason falls back to the shared text");
  const tooLarge = batchErrorText(t, tc, new APIError("BATCH_FILE_TOO_LARGE", { limit_mb: 200 }, "too large", 413));
  assert.equal(tooLarge.message, c.apiError.BATCH_FILE_TOO_LARGE.replace("{{limit_mb}}", "200"));
  const plain = batchErrorText(t, tc, new Error("network down"));
  assert.equal(plain.message, "network down");
});

test("a file over the size limit is refused before any upload", async () => {
  batchApi.list = async () => page([]);
  let uploads = 0;
  batchApi.create = async () => { uploads += 1; return batch(); };
  const user = mountPage();
  const dialog = await openCreate(user);
  await user.upload(within(dialog).getByLabelText(b.create.file), jsonl("huge.jsonl", 201 * 1024 * 1024));
  assert.ok(await within(dialog).findByText(b.create.tooLarge.replace("{{limit}}", "200")));
  assert.equal(submitButton(dialog).disabled, true);
  assert.equal(uploads, 0);
});

test("while uploading, the dialog is locked; a server error then unlocks it", async () => {
  batchApi.list = async () => page([]);
  let fail!: () => void;
  batchApi.create = () => new Promise<Batch>((_resolve, reject) => {
    fail = () => reject(new APIError("BATCH_REQUEST_INVALID", { field: "endpoint", reason: "endpoint_mismatch" }, "endpoint mismatch", 400));
  });
  const user = mountPage();
  const dialog = await openCreate(user);
  await user.upload(within(dialog).getByLabelText(b.create.file), jsonl());
  await user.click(submitButton(dialog));
  await within(dialog).findByText(b.create.submitting);
  assert.equal((within(dialog).getByLabelText(b.create.file) as HTMLInputElement).disabled, true);
  assert.equal((within(dialog).getByRole("button", { name: b.create.cancel }) as HTMLButtonElement).disabled, true);
  escape();
  assert.ok(screen.getByRole("dialog", { name: b.create.title }), "Escape does not abandon an upload in flight");
  fail();
  await within(dialog).findByText(b.errors.requestInvalid.replace("{{field}}", "endpoint").replace("{{reason}}", b.errors.requestReasons.endpoint_mismatch));
  assert.equal(submitButton(dialog).disabled, false);
});

test("the dialog is operable from the keyboard: Tab reaches the controls, Enter submits, Escape closes", async () => {
  batchApi.list = async () => page([]);
  const sent: string[] = [];
  batchApi.create = async (file) => { sent.push(file.name); return batch({ id: "batch_k" }); };
  batchApi.get = async () => batch({ id: "batch_k" });
  const user = mountPage();
  let dialog = await openCreate(user);
  await focusIn(dialog);
  escape();
  await waitFor(() => assert.ok(!screen.queryByRole("dialog", { name: b.create.title })));
  dialog = await openCreate(user);
  const fileInput = within(dialog).getByLabelText(b.create.file);
  fileInput.focus();
  await user.upload(fileInput, jsonl("kb.jsonl"));
  await user.tab();
  assert.ok(document.activeElement === within(dialog).getByRole("combobox", { name: b.create.endpoint }));
  const submit = submitButton(dialog);
  submit.focus();
  await user.keyboard("{Enter}");
  await waitFor(() => assert.deepEqual(sent, ["kb.jsonl"]));
});

test("the dialog can be opened and closed without creating anything", async () => {
  batchApi.list = async () => page([]);
  let uploads = 0;
  batchApi.create = async () => { uploads += 1; return batch(); };
  const user = mountPage();
  const dialog = await openCreate(user);
  await user.click(within(dialog).getByRole("button", { name: b.create.cancel }));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog", { name: b.create.title })));
  assert.equal(uploads, 0);
});

test("the modal component reports nothing while closed", () => {
  render(
    <I18nextProvider i18n={i18n}>
      <BatchCreateModal open={false} onClose={() => {}} onCreated={() => {}} />
    </I18nextProvider>,
  );
  assert.ok(!screen.queryByRole("dialog"));
});

import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
const { render, screen, fireEvent, waitFor, cleanup, act } = await import("@testing-library/react");
const { FeedbackHost, message, confirm } = await import("../src/ui-kit/feedback/index.tsx");
const { i18n } = await createI18n([]);
afterEach(() => { cleanup(); message.destroy(); });
const mount = () => render(<I18nextProvider i18n={i18n}><FeedbackHost /></I18nextProvider>);

test("keyed messages update and can be explicitly dismissed", async () => {
  mount();
  act(() => { message.info({ content: "Working", key: "work", duration: 0 }); });
  await screen.findByText("Working");
  act(() => { message.success({ content: "Done", key: "work", duration: 0 }); });
  await screen.findByText("Done", {}, { mutationObserverOptions: { childList: true, subtree: true } });
  await waitFor(() => assert.ok(!screen.queryByText("Working")));
  act(() => { message.destroy("work"); });
  await waitFor(() => assert.ok(!screen.queryByText("Done")), { mutationObserverOptions: { childList: true, subtree: true } });
});
test("confirm blocks dismissal during submission and preserves failed context for retry", async () => {
  mount();
  let reject!: (reason: Error) => void;
  let calls = 0;
  let cancelled = false;
  act(() => { confirm({ title: "Remove resource", content: "Existing data", onCancel: () => { cancelled = true; }, onOk: () => ++calls === 1 ? new Promise((_, no) => { reject = no; }) : Promise.resolve() }); });
  fireEvent.click(await screen.findByRole("button", { name: "Confirm" }));
  assert.equal((screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled, true);
  fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
  assert.equal(cancelled, false);
  await act(async () => reject(new Error("Server unavailable")));
  await screen.findByRole("alert");
  assert.ok(screen.getByText("Existing data"));
  fireEvent.click(screen.getByRole("button", { name: "Confirm" }));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog")));
  assert.equal(calls, 2);
});
test("confirm cancel invokes its callback without accepting", async () => {
  mount();
  let cancelled = false;
  let accepted = false;
  act(() => { confirm({ title: "Cancel action", onOk: () => { accepted = true; }, onCancel: () => { cancelled = true; } }); });
  fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog")));
  assert.equal(cancelled, true);
  assert.equal(accepted, false);
});

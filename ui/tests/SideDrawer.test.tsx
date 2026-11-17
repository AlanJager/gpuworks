import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React, { useState } from "react";
import { I18nextProvider } from "react-i18next";

const { render, screen, within, waitFor, cleanup, fireEvent } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { SideDrawer } = await import("../src/ui-kit/SideDrawer.tsx");
const { AssistantProvider, useAssistant } = await import("../src/assistant/AssistantContext.tsx");
const { default: AssistantPanel } = await import("../src/assistant/AssistantPanel.tsx");
const { i18n } = await createI18n(["ai"]);
const originalFetch = globalThis.fetch;
afterEach(() => { cleanup(); globalThis.fetch = originalFetch; });

function Example({ nativeAssistant = false }: { nativeAssistant?: boolean }) {
  const [open, setOpen] = useState(false);
  const assistant = useAssistant();
  return <>
    <button onClick={() => setOpen(true)}>Inspect node</button>
    <button onClick={() => assistant.setOpen(true)}>Open assistant</button>
    <SideDrawer open={open} onClose={() => setOpen(false)} title="Node detail">
      <button onClick={() => assistant.setOpen(true)}>Diagnose node</button>
      <input aria-label="Node note" />
    </SideDrawer>
    {nativeAssistant ? assistant.open && <div className="assistant-panel"><input aria-label="Assistant editor" /></div> : <AssistantPanel />}
  </>;
}

for (const assistantFirst of [false, true]) {
  test(`assistant and detail accept input without dismissing each other (${assistantFirst ? "assistant" : "detail"} first)`, async () => {
    globalThis.fetch = (async () => new Response(JSON.stringify({ id: 1, available: false }), { headers: { "Content-Type": "application/json" } })) as typeof fetch;
    render(<I18nextProvider i18n={i18n}><AssistantProvider><Example /></AssistantProvider></I18nextProvider>);
    const user = userEvent.setup();
    if (assistantFirst) await user.click(screen.getByRole("button", { name: "Open assistant" }));
    await user.click(screen.getByRole("button", { name: "Inspect node" }));
    if (!assistantFirst) await user.click(await screen.findByRole("button", { name: "Diagnose node" }));
    const question = await screen.findByRole("textbox", { name: "Question" });
    await user.click(question);
    await user.type(question, "Check this node");
    assert.equal(document.activeElement, question);
    assert.equal((question as HTMLTextAreaElement).value, "Check this node");
    const note = screen.getByRole("textbox", { name: "Node note" });
    await user.click(note);
    await user.type(note, "Keep this detail");
    assert.equal(document.activeElement, note);
    assert.ok(screen.getByRole("dialog", { name: /AI Assistant/ }));
    await user.click(question);
    // rc-drawer uses the native legacy keyCode, which jsdom userEvent omits.
    fireEvent.keyDown(question, { key: "Escape", keyCode: 27 });
    await waitFor(() => assert.equal(document.body.classList.contains("assistant-open"), false));
    const detail = screen.getByRole("dialog", { name: "Node detail" });
    assert.equal((within(detail).getByRole("textbox", { name: "Node note" }) as HTMLInputElement).value, "Keep this detail");
    await user.click(within(detail).getByRole("button", { name: "Close" }));
    await waitFor(() => assert.equal(screen.queryByRole("dialog", { name: "Node detail" }), null));
  });
}

test("the detail backdrop still dismisses the detail while the assistant stays open", async () => {
  globalThis.fetch = (async () => new Response(JSON.stringify({ id: 1, available: false }), { headers: { "Content-Type": "application/json" } })) as typeof fetch;
  render(<I18nextProvider i18n={i18n}><AssistantProvider><Example /></AssistantProvider></I18nextProvider>);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Inspect node" }));
  await user.click(await screen.findByRole("button", { name: "Diagnose node" }));
  const question = await screen.findByRole("textbox", { name: "Question" });
  await user.click(question);
  const backdrop = document.querySelector<HTMLElement>(".side-drawer-backdrop");
  assert.ok(backdrop);
  await user.click(backdrop);
  await waitFor(() => assert.equal(screen.queryByRole("dialog", { name: "Node detail" }), null));
  assert.ok(screen.getByRole("dialog", { name: /AI Assistant/ }));
});

test("Escape in the assistant remains available to its own keyboard handler", async () => {
  globalThis.fetch = (async () => new Response(JSON.stringify({ id: 1, available: false }), { headers: { "Content-Type": "application/json" } })) as typeof fetch;
  render(<I18nextProvider i18n={i18n}><AssistantProvider><Example nativeAssistant /></AssistantProvider></I18nextProvider>);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Inspect node" }));
  await user.click(await screen.findByRole("button", { name: "Diagnose node" }));
  const editor = screen.getByRole("textbox", { name: "Assistant editor" });
  await user.click(editor);
  let received = false;
  const handle = (event: KeyboardEvent) => { if (event.key === "Escape") received = true; };
  window.addEventListener("keydown", handle);
  try {
    const escape = new window.KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true });
    editor.dispatchEvent(escape);
    assert.equal(escape.defaultPrevented, false);
    assert.equal(received, true);
    assert.ok(screen.getByRole("dialog", { name: "Node detail" }));
  } finally {
    window.removeEventListener("keydown", handle);
  }
});

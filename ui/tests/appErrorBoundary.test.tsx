import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";

const { i18n, locale } = await createI18n([]);
const { cleanup, render, screen } = await import("@testing-library/react");
const { default: AppErrorBoundary } = await import("../src/components/AppErrorBoundary.tsx");
afterEach(() => cleanup());
after(() => dom.window.close());

function Broken(): React.ReactElement { throw new Error("boom"); }

test("a page that renders shows through the boundary unchanged", () => {
  render(<I18nextProvider i18n={i18n}><AppErrorBoundary><p>page body</p></AppErrorBoundary></I18nextProvider>);
  assert.ok(screen.getByText("page body"));
});

// A render failure replaces the page with what happened and one way out.
test("a page that fails to render shows the failure with a refresh action", () => {
  const original = console.error;
  console.error = () => {};
  try {
    render(<I18nextProvider i18n={i18n}><AppErrorBoundary><Broken /></AppErrorBoundary></I18nextProvider>);
  } finally {
    console.error = original;
  }
  assert.ok(screen.getByText(locale.common.errorBoundary.title));
  assert.ok(screen.getByText(locale.common.errorBoundary.description));
  const refresh = screen.getByRole("button", { name: locale.common.actions.refresh });
  refresh.focus();
  assert.equal(document.activeElement, refresh, "the refresh action is reachable by keyboard");
});

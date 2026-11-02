import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";

// The playground's notice for a sleeping model says what the next request
// does, as the server tells it: a platform sleep ends on a request, so sending
// is enough; an operator's or a schedule's sleep does not, so the model must be
// woken first, by the page's own button or from Deployments.

void dom;
const { i18n, locale } = await createI18n(["playground"]);
const { cleanup, render, screen } = await import("@testing-library/react");
const { default: SleepNotice } = await import("../src/pages/playground/SleepNotice.tsx");
type Model = import("../src/pages/playground/types.ts").PlaygroundModel;

const s = locale.playground.sleep;
const model = (overrides: Partial<Model>): Model => ({ id: "m", label: "m", capability: "chat", operations: [], sleeping: false, public_data_plane: true, ...overrides });
const mount = (node: React.ReactNode) => render(<I18nextProvider i18n={i18n}>{node}</I18nextProvider>);

afterEach(() => cleanup());

test("an awake model, or none, shows nothing", () => {
  const { container } = mount(<><SleepNotice model={model({})} /><SleepNotice /></>);
  assert.equal(container.textContent, "");
});

test("a sleep a request ends says sending wakes it, with no wake to press", () => {
  mount(<SleepNotice model={model({ sleeping: true, wakes_on_request: true })} action={<button type="button">wake</button>} />);
  assert.ok(screen.getByText(s.wakesOnRequest));
  assert.equal(screen.queryByRole("button", { name: "wake" }), null);
});

test("a held sleep asks for a wake: the page's own, or from Deployments", () => {
  mount(<SleepNotice model={model({ sleeping: true })} action={<button type="button">wake</button>} />);
  assert.ok(screen.getByText(s.held));
  assert.ok(screen.getByRole("button", { name: "wake" }));
  cleanup();
  mount(<SleepNotice model={model({ sleeping: true, wakes_on_request: false })} />);
  assert.ok(screen.getByText(s.heldElsewhere));
});

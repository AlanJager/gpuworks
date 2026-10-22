import { dom, createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { installPullFake } from "./helpers/pullFake.ts";

const { i18n, locale } = await createI18n(["settings", "nodes"]);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const PullSettingsCard = (await import("../src/pages/settings/pull/PullSettingsCard.tsx")).default;

const text = locale.settings.pull as Record<string, any>;
const fakes: { restore(): void }[] = [];
afterEach(() => {
  cleanup();
  while (fakes.length) fakes.pop()!.restore();
});

test("the card opens on source rules and switches tabs by keyboard", async () => {
  fakes.push(installPullFake(api as unknown as Record<string, unknown>, {
    rules: [{ id: 1, match: "docker.io", endpoints: ["mirror.example.com"], allow_upstream: true, note: "", node_requirements: {} }],
    groups: [{ id: 1, name: "default-all-nodes", node_requirements: {}, rate_limit_kib_s: 0 }],
    credentials: [{ id: 1, host: "harbor.corp.example", username: "ci", has_secret: true }],
  }));
  render(<I18nextProvider i18n={i18n}><PullSettingsCard /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  assert.ok(screen.getByText(text.title));
  const tabs = screen.getAllByRole("tab");
  assert.deepEqual(tabs.map((tab) => tab.textContent), [text.tabs.rules, text.tabs.proxies, text.tabs.groups, text.tabs.credentials]);
  assert.equal(tabs[0].getAttribute("aria-selected"), "true");
  await screen.findByText("mirror.example.com");

  // Keyboard: ArrowRight moves the tab focus, Enter selects the focused tab (rc-tabs reads `code`).
  tabs[0].focus();
  fireEvent.keyDown(tabs[0], { key: "ArrowRight", code: "ArrowRight" });
  fireEvent.keyDown(tabs[0], { key: "Enter", code: "Enter" });
  await waitFor(() => assert.equal(screen.getAllByRole("tab")[1].getAttribute("aria-selected"), "true"));
  await screen.findByText(text.proxies.empty);
  await user.click(screen.getAllByRole("tab")[2]);
  await waitFor(() => assert.ok(screen.getAllByText("default-all-nodes").length > 0));
  await user.click(screen.getAllByRole("tab")[3]);
  await screen.findByText("harbor.corp.example");
  assert.ok(within(screen.getByRole("tabpanel")).getByText(text.credentials.intro));
});

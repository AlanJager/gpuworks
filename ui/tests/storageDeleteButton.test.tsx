import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { createI18n, dom } from "./helpers/dom.ts";
import { localStorage } from "./helpers/storageFixtures.ts";
import { I18nextProvider } from "react-i18next";

const { i18n, locale } = await createI18n(["storage"]);
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { ConfigProvider } = await import("antd");
const { default: StorageDeleteButton } = await import("../src/pages/storage/StorageDeleteButton.tsx");
const s = locale.storage;
afterEach(cleanup);

function setup() {
  let confirmed = 0;
  render(
    <I18nextProvider i18n={i18n}>
      <ConfigProvider theme={{ token: { motion: false } }}>
        <StorageDeleteButton link storage={localStorage({ name: "test-storage" })} onConfirm={() => { confirmed += 1; }} />
      </ConfigProvider>
    </I18nextProvider>,
  );
  return {
    user: userEvent.setup({ document: dom.window.document }),
    trigger: screen.getByRole("button", { name: s.actions.delete }),
    confirmed: () => confirmed,
  };
}

function hoverTooltips() {
  return screen.queryAllByRole("tooltip").filter((tooltip) => tooltip.textContent === s.actions.delete);
}

async function confirmation() {
  const title = s.actions.deleteConfirm.replace("{{name}}", "test-storage");
  await screen.findByText(title);
  return screen.getAllByRole("tooltip").find((tooltip) => tooltip.textContent?.includes(title))!;
}

test("opening storage deletion dismisses its hover tooltip; cancel preserves the resource and restores hover", async () => {
  const { user, trigger, confirmed } = setup();
  await user.hover(trigger);
  await waitFor(() => assert.equal(hoverTooltips().length, 1));
  await user.click(trigger);
  const popup = await confirmation();
  await waitFor(() => assert.equal(hoverTooltips().length, 0, "hover text must not cover confirmation actions"));
  await user.click(within(popup).getByRole("button", { name: s.actions.cancel }));
  await waitFor(() => assert.ok(!screen.queryAllByRole("tooltip").some((tooltip) => tooltip.textContent?.includes("test-storage"))));
  assert.equal(confirmed(), 0);
  await user.hover(trigger);
  await waitFor(() => assert.equal(hoverTooltips().length, 1));
  assert.equal(confirmed(), 0);
});

test("confirming storage deletion calls the mutation once", async () => {
  const { user, trigger, confirmed } = setup();
  await user.click(trigger);
  const popup = await confirmation();
  await user.click(within(popup).getByRole("button", { name: s.actions.delete }));
  assert.equal(confirmed(), 1);
});

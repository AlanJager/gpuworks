import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React, { useState } from "react";
import { dom } from "./helpers/dom.ts";
import { i18n, systemVersion, zhImages } from "./helpers/imagesHarness.ts";
import { I18nextProvider } from "react-i18next";
import type { ImageCatalogEntry } from "../src/api.ts";

const { cleanup, render, screen } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { Table } = await import("antd");
const { buildVersionColumns } = await import("../src/pages/images/ImageVersionColumns.tsx");
afterEach(cleanup);

test("a disabled version remains editable and a parent cycle preserves mixed child settings", async () => {
  const changes: Array<{ id: number; enabled: boolean }> = [];
  function Versions({ seriesEnabled }: { seriesEnabled: boolean }) {
    const [enabled, setEnabled] = useState([true, false]);
    // Match the real API: effective state is the conjunction; own state survives
    // parent disable/enable. A false effective state cannot identify its cause.
    const rows: ImageCatalogEntry[] = enabled.map((value, index) => ({ ...systemVersion,
      id: 11 + index, image_ref: `example/runner:${index}`, enabled: value,
      effective_enabled: value && seriesEnabled }));
    const columns = buildVersionColumns({ seriesEnabled, onSelect: () => {}, onToggle: (row, value) => {
      changes.push({ id: row.id, enabled: value });
      setEnabled((previous) => previous.map((old, index) => 11 + index === row.id ? value : old));
    } }, i18n.getFixedT("zh", "images"));
    return <Table rowKey="id" columns={columns} dataSource={rows} pagination={false} />;
  }
  const view = (parent: boolean) => <I18nextProvider i18n={i18n}><Versions seriesEnabled={parent} /></I18nextProvider>;
  const mounted = render(view(true));
  const user = userEvent.setup({ document: dom.window.document });
  const control = (index: number) => screen.getByRole("switch", {
    name: zhImages.versionTable.enabledLabel.replace("{{ref}}", `example/runner:${index}`),
  }) as HTMLButtonElement;
  await user.click(control(0));
  assert.equal(control(0).getAttribute("aria-checked"), "false");
  assert.equal(control(0).disabled, false, "a version disabled by itself must remain editable");
  assert.equal(screen.queryByText(zhImages.versionTable.disabledBySeries), null);
  await user.click(control(0));
  assert.equal(control(0).getAttribute("aria-checked"), "true");
  mounted.rerender(view(false));
  for (const index of [0, 1]) {
    assert.equal(control(index).disabled, true);
    assert.equal(control(index).getAttribute("aria-checked"), "false");
  }
  assert.equal(screen.getAllByText(zhImages.versionTable.disabledBySeries).length, 2);
  mounted.rerender(view(true));
  for (const index of [0, 1]) {
    assert.equal(control(index).disabled, false);
    assert.equal(control(index).getAttribute("aria-checked"), String(index === 0));
  }
  assert.equal(screen.queryByText(zhImages.versionTable.disabledBySeries), null);
  assert.deepEqual(changes, [{ id: 11, enabled: false }, { id: 11, enabled: true }]);
});

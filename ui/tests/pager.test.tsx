import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React, { useState } from "react";
import { I18nextProvider } from "react-i18next";
const { render, screen, cleanup } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { Pager } = await import("../src/ui-kit/Pager.tsx");

afterEach(cleanup);

async function mount(node: React.ReactNode, lng: "en" | "zh" = "en") {
  const { i18n } = await createI18n([], lng);
  return render(<I18nextProvider i18n={i18n}>{node}</I18nextProvider>);
}

function Example({ start = 1, total = 45, onChange }: { start?: number; total?: number; onChange?: (page: number) => void }) {
  const [page, setPage] = useState(start);
  return <Pager page={page} pageSize={20} total={total} onChange={(p) => { onChange?.(p); setPage(p); }} />;
}

test("the pager shows the total and the page, and offers no page-size choice", async () => {
  await mount(<Example />);
  assert.ok(screen.getByText("45 items"));
  assert.ok(screen.getByText("Page 1 of 3"));
  assert.equal(screen.queryByRole("combobox"), null);
  assert.equal(screen.queryByText("Rows per page"), null);
  assert.equal(screen.getByRole("button", { name: "First page" }).hasAttribute("disabled"), true);
  assert.equal(screen.getByRole("button", { name: "Previous page" }).hasAttribute("disabled"), true);
  assert.equal(screen.getByRole("button", { name: "Next page" }).hasAttribute("disabled"), false);
});

test("next, last, previous and first move through the pages, by click and by keyboard", async () => {
  const seen: number[] = [];
  await mount(<Example onChange={(p) => seen.push(p)} />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Next page" }));
  assert.ok(screen.getByText("Page 2 of 3"));
  await user.click(screen.getByRole("button", { name: "Last page" }));
  assert.ok(screen.getByText("Page 3 of 3"));
  assert.equal(screen.getByRole("button", { name: "Next page" }).hasAttribute("disabled"), true);
  screen.getByRole("button", { name: "Previous page" }).focus();
  await user.keyboard("{Enter}");
  assert.ok(screen.getByText("Page 2 of 3"));
  screen.getByRole("button", { name: "First page" }).focus();
  await user.keyboard(" ");
  assert.ok(screen.getByText("Page 1 of 3"));
  assert.deepEqual(seen, [2, 3, 2, 1]);
});

test("a page past the end shows the last page, and an empty list shows one page", async () => {
  await mount(<Example start={9} />);
  assert.ok(screen.getByText("Page 3 of 3"));
  cleanup();
  await mount(<Example total={0} />, "zh");
  assert.ok(screen.getByText("共 0 项"));
  assert.ok(screen.getByText("第 1 页，共 1 页"));
  assert.equal(screen.getByRole("button", { name: "最后一页" }).hasAttribute("disabled"), true);
});

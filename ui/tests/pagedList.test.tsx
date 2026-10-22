import assert from "node:assert/strict";
import { after, test } from "node:test";
import { JSDOM } from "jsdom";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "Element", "Node", "MutationObserver", "HTMLBodyElement"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}

const { act, renderHook, waitFor } = await import("@testing-library/react");
const { fetchAllPages, usePagedList } = await import("../src/pagedList.ts");
import type { ListFilters } from "../src/pagedList";
import type { ListPage, ListQuery } from "../src/api";

after(() => dom.window.close());

// A list of 95 rows the fake pages through as the server does.
function server() {
  const queries: ListQuery[] = [];
  const fetchPage = async (query: ListQuery): Promise<ListPage<number>> => {
    queries.push(query);
    const rows = Array.from({ length: 95 }, (_, i) => i + 1).filter((row) => !query.q || String(row).includes(query.q));
    const start = (query.page - 1) * query.page_size;
    return { items: rows.slice(start, start + query.page_size), page: query.page, page_size: query.page_size, total: rows.length };
  };
  return { queries, fetchPage };
}

test("the hook pages, and a new filter or page size starts again from page 1", async () => {
  const { queries, fetchPage } = server();
  const view = renderHook(({ filters }: { filters: ListFilters }) => usePagedList(fetchPage, { filters, pageSize: 20 }), { initialProps: { filters: {} } });
  await waitFor(() => assert.equal(view.result.current.data?.total, 95));
  assert.deepEqual(queries.at(-1), { page: 1, page_size: 20 });
  assert.deepEqual(view.result.current.pagination.total, 95);

  act(() => view.result.current.pagination.onChange(3, 20));
  await waitFor(() => assert.deepEqual(view.result.current.data?.items[0], 41));
  assert.equal(view.result.current.pagination.current, 3);

  // A new filter reads page 1, never the old page under the new filter.
  view.rerender({ filters: { q: "9" } });
  assert.equal(view.result.current.page, 1);
  await waitFor(() => assert.deepEqual(queries.at(-1), { q: "9", page: 1, page_size: 20 }));
  assert.ok(!queries.some((query) => query.q === "9" && query.page === 3), "a stale page was requested under the new filter");

  act(() => view.result.current.pagination.onChange(2, 20));
  await waitFor(() => assert.equal(view.result.current.page, 2));
  // A new page size starts from the first page too.
  act(() => view.result.current.pagination.onChange(2, 50));
  await waitFor(() => assert.deepEqual(queries.at(-1), { q: "9", page: 1, page_size: 50 }));
  view.unmount();
});

test("fetchAllPages reads the first page, then the rest together", async () => {
  const { queries, fetchPage } = server();
  const rows = await fetchAllPages(fetchPage, { q: "1" }, 10);
  assert.deepEqual(rows, Array.from({ length: 95 }, (_, i) => i + 1).filter((row) => String(row).includes("1")));
  assert.deepEqual(queries.map((query) => query.page), [1, 2]);
  assert.ok(queries.every((query) => query.q === "1" && query.page_size === 10));

  const one = server();
  assert.equal((await fetchAllPages(one.fetchPage, {}, 200)).length, 95);
  assert.equal(one.queries.length, 1, "a list that fits one page is read once");
});

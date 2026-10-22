import type { CursorPage, List, ListPage } from "../../src/api";

// list wraps fixture rows as an unpaged collection answers them:
// {items, total} with total = items.length (docs/reference/api-conventions.md).
export function list<T>(items: T[]): List<T> {
  return { items, total: items.length };
}

// cursorPage wraps fixture rows as one page of a cursor-paged collection
// (the event timelines): {items}, no total.
export function cursorPage<T>(items: T[]): CursorPage<T> {
  return { items };
}

// listPage wraps fixture rows as the single page a server-paged list returns.
export function listPage<T>(items: T[], page = 1, pageSize = 20): ListPage<T> {
  return { items, page, page_size: pageSize, total: items.length };
}

// paged turns a fixture fetcher of rows into a paged-list fetcher.
export function paged<T>(fetch: () => Promise<T[]>): () => Promise<ListPage<T>> {
  return async () => listPage(await fetch());
}

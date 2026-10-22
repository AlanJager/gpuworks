import { useState } from "react";
import { usePolling, type ListPage, type ListQuery } from "./api";

/** The filters of a paged list query: everything but the page. */
export type ListFilters = Omit<ListQuery, "page" | "page_size">;

/**
 * usePagedList polls one page of a server-paged list (GET /deployments,
 * GET /nodes, ... with page parameters) and owns the page and page size. A
 * change of filters starts again from the first page; so does a new page
 * size. `pagination` plugs straight into DataTable/antd Table.
 */
export function usePagedList<T, P extends ListPage<T> = ListPage<T>>(
  fetchPage: (query: ListQuery) => Promise<P>,
  { filters = {}, pageSize: initialPageSize = 20, intervalMs = 0 }: { filters?: ListFilters; pageSize?: number; intervalMs?: number } = {},
) {
  const filtersKey = JSON.stringify([filters.q?.trim() ?? "", filters.state ?? [], filters.label ?? [], filters.sort ?? "", filters.extra ?? {}]);
  // The page belongs to the filters it was chosen under: new filters read
  // as page 1 without a render on the stale page first.
  const [paging, setPaging] = useState({ page: 1, pageSize: initialPageSize, filtersKey });
  const page = paging.filtersKey === filtersKey ? paging.page : 1;
  const pageSize = paging.pageSize;
  const query: ListQuery = { ...filters, page, page_size: pageSize };
  const list = usePolling<P>(() => fetchPage(query), intervalMs, [filtersKey, page, pageSize]);
  return {
    ...list,
    page,
    pageSize,
    pagination: {
      current: page,
      pageSize,
      total: list.data?.total ?? 0,
      showSizeChanger: true,
      onChange: (next: number, size: number) =>
        setPaging({ page: size !== pageSize ? 1 : next, pageSize: size, filtersKey }),
    },
  };
}

/**
 * fetchAllPages reads every page of a paged list, for a list small enough to
 * hold whole (the inference template catalog): the first page says how many
 * there are, the rest are fetched together.
 */
export async function fetchAllPages<T>(fetchPage: (query: ListQuery) => Promise<ListPage<T>>, filters: ListFilters = {}, pageSize = 200): Promise<T[]> {
  const first = await fetchPage({ ...filters, page: 1, page_size: pageSize });
  const pages = Math.ceil(first.total / first.page_size);
  if (pages <= 1) return first.items;
  const rest = await Promise.all(Array.from({ length: pages - 1 }, (_, index) => fetchPage({ ...filters, page: index + 2, page_size: pageSize })));
  return [first, ...rest].flatMap((page) => page.items);
}

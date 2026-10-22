/**
 * Column preferences of one list table: which columns show, in which order,
 * how wide, and how many rows a page holds. They are a viewer's convenience
 * kept in this browser only; anything unreadable falls back to the defaults.
 */

/** What a preference needs to know about a column. */
export interface PrefColumn {
  key: string;
  /** The name column leads and the actions column trails; neither hides nor moves. */
  role?: "name" | "actions";
  defaultHidden?: boolean;
  width: number;
  minWidth?: number;
}

export interface ColumnPrefs {
  /** Every movable column, in display order (the name and actions columns are not listed). */
  order: string[];
  hidden: string[];
  widths: Record<string, number>;
  pageSize?: number;
}

const STORAGE_PREFIX = "gpuworks.table.";
export const DEFAULT_MIN_COLUMN_WIDTH = 64;
export const PAGE_SIZE_OPTIONS = [10, 20, 50, 100];

export function storageKey(tableId: string): string {
  return `${STORAGE_PREFIX}${tableId}.v1`;
}

export function minWidthOf(column: PrefColumn): number {
  return column.minWidth ?? Math.min(column.width, DEFAULT_MIN_COLUMN_WIDTH);
}

function movable(columns: PrefColumn[]): PrefColumn[] {
  return columns.filter((column) => !column.role);
}

export function defaultColumnPrefs(columns: PrefColumn[]): ColumnPrefs {
  const free = movable(columns);
  return { order: free.map((column) => column.key), hidden: free.filter((column) => column.defaultHidden).map((column) => column.key), widths: {} };
}

/**
 * parseColumnPrefs restores a saved layout against the columns the page has
 * now. The viewer's order, visibility and widths survive for the columns it
 * knows; a column it has never seen is inserted after its default predecessor
 * with its default visibility, and a column that no longer exists is dropped.
 */
export function parseColumnPrefs(raw: string | null | undefined, columns: PrefColumn[]): ColumnPrefs {
  const fallback = defaultColumnPrefs(columns);
  if (!raw) return fallback;
  let parsed: Partial<Record<keyof ColumnPrefs, unknown>>;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return fallback;
  }
  if (!parsed || typeof parsed !== "object") return fallback;
  const pageSize = typeof parsed.pageSize === "number" && PAGE_SIZE_OPTIONS.includes(parsed.pageSize) ? parsed.pageSize : undefined;
  // A layout holding only a page size (saved before any column was touched) keeps it.
  if (!Array.isArray(parsed.order)) return pageSize ? { ...fallback, pageSize } : fallback;
  const free = movable(columns);
  const known = new Map(free.map((column) => [column.key, column]));
  const order = [...new Set(parsed.order.filter((key): key is string => typeof key === "string" && known.has(key)))];
  const seen = new Set(order);
  const hiddenSaved = Array.isArray(parsed.hidden) ? parsed.hidden.filter((key): key is string => typeof key === "string" && known.has(key)) : [];
  const hidden = new Set(hiddenSaved);
  free.forEach((column, index) => {
    if (seen.has(column.key)) return;
    const previous = index > 0 ? order.indexOf(free[index - 1].key) : -1;
    order.splice(previous + 1, 0, column.key);
    if (column.defaultHidden) hidden.add(column.key);
  });
  const widths: Record<string, number> = {};
  const allColumns = new Map(columns.map((column) => [column.key, column]));
  if (parsed.widths && typeof parsed.widths === "object") {
    for (const [key, value] of Object.entries(parsed.widths as Record<string, unknown>)) {
      const column = allColumns.get(key);
      if (column && typeof value === "number" && Number.isFinite(value)) widths[key] = Math.max(minWidthOf(column), Math.round(value));
    }
  }
  return { order, hidden: order.filter((key) => hidden.has(key)), widths, ...(pageSize ? { pageSize } : {}) };
}

/** The columns as displayed: the name column, the visible movable columns in order, the actions column. */
export function arrangeColumns<C extends PrefColumn>(columns: C[], prefs: ColumnPrefs): C[] {
  const byKey = new Map(columns.map((column) => [column.key, column]));
  const hidden = new Set(prefs.hidden);
  const middle = prefs.order.filter((key) => !hidden.has(key)).map((key) => byKey.get(key)).filter((column): column is C => !!column);
  return [...columns.filter((column) => column.role === "name"), ...middle, ...columns.filter((column) => column.role === "actions")];
}

export function widthOf(column: PrefColumn, prefs: ColumnPrefs): number {
  return prefs.widths[column.key] ?? column.width;
}

export function loadColumnPrefs(tableId: string, columns: PrefColumn[]): ColumnPrefs {
  try {
    return parseColumnPrefs(window.localStorage.getItem(storageKey(tableId)), columns);
  } catch {
    return defaultColumnPrefs(columns);
  }
}

export function saveColumnPrefs(tableId: string, prefs: ColumnPrefs): void {
  try {
    window.localStorage.setItem(storageKey(tableId), JSON.stringify(prefs));
  } catch {
    // Storage may be unavailable (private window, blocked site data); the
    // layout then lasts as long as the page.
  }
}

/** saveStoredPageSize remembers a table's page size and leaves the rest of its saved layout as it is. */
export function saveStoredPageSize(tableId: string, pageSize: number): void {
  try {
    const key = storageKey(tableId);
    let saved: Record<string, unknown> = {};
    try {
      const parsed = JSON.parse(window.localStorage.getItem(key) ?? "{}");
      if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) saved = parsed;
    } catch {
      // An unreadable layout is replaced by the page size alone.
    }
    window.localStorage.setItem(key, JSON.stringify({ ...saved, pageSize }));
  } catch {
    // Storage may be unavailable; the size lasts as long as the page.
  }
}

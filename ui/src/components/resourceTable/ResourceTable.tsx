import { useEffect, useMemo, useState, type ReactNode } from "react";
import { Alert, Button, Space, Table, Tooltip, Typography } from "antd";
import type { TableProps } from "antd";
import type { ColumnType, SortOrder, TablePaginationConfig } from "antd/es/table/interface";
import { ReloadOutlined, SettingOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import EmptyState, { type EmptyStateKind } from "../EmptyState";
import { formatTime } from "../../i18n/datetime";
import ColumnSettingsDrawer from "./ColumnSettingsDrawer";
import ResizableHeaderCell, { type ResizeProps } from "./ResizableHeaderCell";
import {
  PAGE_SIZE_OPTIONS,
  arrangeColumns,
  defaultColumnPrefs,
  loadColumnPrefs,
  minWidthOf,
  saveColumnPrefs,
  widthOf,
  type ColumnPrefs,
} from "./columnPrefs";

export type SortValue = string | number | null | undefined;

/**
 * One column of a resource list. On top of an antd column it names a stable
 * key (preferences are saved under it), a default width, and its role: the
 * name column leads and freezes left, the actions column trails and freezes
 * right, and neither hides nor moves.
 */
export type ResourceColumn<T> = Omit<ColumnType<T>, "key" | "width" | "fixed" | "sorter" | "sortOrder"> & {
  key: string;
  width: number;
  minWidth?: number;
  /** The column's name in the settings drawer when the title is not plain text. */
  label?: string;
  role?: "name" | "actions";
  defaultHidden?: boolean;
  /** Sorts a client-side list by this value. */
  sortValue?: (row: T) => SortValue;
  /** The list is sorted by the server under this column's key. */
  serverSort?: boolean;
};

export interface ServerSort {
  key: string;
  order: "asc" | "desc";
}

/** The page size a table last used in this browser, for a server-paged list's first request. */
export function storedPageSize(tableId: string, fallback: number): number {
  return loadColumnPrefs(tableId, []).pageSize ?? fallback;
}

export function compareSortValues(a: SortValue, b: SortValue): number {
  const missingA = a == null || a === "";
  const missingB = b == null || b === "";
  if (missingA || missingB) return missingA === missingB ? 0 : missingA ? -1 : 1;
  if (typeof a === "number" && typeof b === "number") return a - b;
  return String(a).localeCompare(String(b), undefined, { numeric: true, sensitivity: "base" });
}

function toAntdOrder(order?: "asc" | "desc"): SortOrder {
  return order === "asc" ? "ascend" : order === "desc" ? "descend" : null;
}

export default function ResourceTable<T extends object>({
  tableId,
  columns,
  data,
  rowKey,
  loading,
  error,
  onRefresh,
  updatedAt,
  toolbar,
  toolbarExtra,
  pagination,
  sort,
  onSortChange,
  onFiltersChange,
  rowSelection,
  expandable,
  onRow,
  emptyDescription,
  emptyKind = "data",
  emptyAction,
  columnSettings = true,
}: {
  /** Names the saved column layout; unique per list. */
  tableId: string;
  columns: ResourceColumn<T>[];
  data?: T[];
  rowKey: TableProps<T>["rowKey"];
  loading?: boolean;
  error?: Error | null;
  onRefresh?: () => void;
  updatedAt?: Date | null;
  /** Search and filters, left of the toolbar. */
  toolbar?: ReactNode;
  /** Extra controls right of the toolbar, before refresh and column settings. */
  toolbarExtra?: ReactNode;
  /** A server-paged list's pagination; omitted, the table pages the rows it is given. */
  pagination?: TablePaginationConfig | false;
  /** A server-sorted list's sort; with onSortChange, columns marked serverSort sort through the server. */
  sort?: ServerSort | null;
  onSortChange?: (sort: ServerSort | null) => void;
  /** A server-filtered list's header filters, by column key. */
  onFiltersChange?: (filters: Record<string, string[]>) => void;
  rowSelection?: TableProps<T>["rowSelection"];
  expandable?: TableProps<T>["expandable"];
  onRow?: TableProps<T>["onRow"];
  emptyDescription?: string;
  emptyKind?: EmptyStateKind;
  emptyAction?: ReactNode;
  /** The column settings button; a table that keeps only its key columns (a detail drawer's) has none. */
  columnSettings?: boolean;
}) {
  const { t } = useTranslation("common");
  const [prefs, setPrefs] = useState<ColumnPrefs>(() => loadColumnPrefs(tableId, columns));
  const [liveWidths, setLiveWidths] = useState<Record<string, number>>({});
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [clientPage, setClientPage] = useState(1);
  const columnKeys = columns.map((column) => column.key).join("|");
  // A page whose column set changes (a column shown only for some data)
  // re-reads the saved layout against the new set; the keys say when.
  useEffect(() => {
    setPrefs(loadColumnPrefs(tableId, columns));
  }, [tableId, columnKeys]);

  const update = (next: ColumnPrefs) => {
    setPrefs(next);
    saveColumnPrefs(tableId, next);
  };
  const reset = () => update({ ...defaultColumnPrefs(columns), ...(prefs.pageSize ? { pageSize: prefs.pageSize } : {}) });

  const arranged = useMemo(() => arrangeColumns(columns, prefs), [columns, prefs]);
  const effectiveWidth = (column: ResourceColumn<T>) => liveWidths[column.key] ?? widthOf(column, prefs);
  const serverSorting = !!onSortChange;

  const antdColumns: ColumnType<T>[] = arranged.map((column) => {
    const { role, sortValue, serverSort, defaultHidden: _hidden, label, minWidth: _min, width: _width, ...rest } = column;
    const width = effectiveWidth(column);
    const title = column.title ?? (role === "actions" ? t("resourceTable.actions") : undefined);
    const name = label ?? (typeof title === "string" ? title : undefined);
    const resize: ResizeProps = {
      resizeWidth: width,
      resizeMin: minWidthOf(column),
      resizeLabel: t("resourceTable.resizeColumn", { name: name ?? column.key }),
      headerLabel: name,
      onResize: (next) => setLiveWidths((current) => ({ ...current, [column.key]: next })),
      onResizeEnd: (next) => {
        setLiveWidths((current) => {
          const { [column.key]: _done, ...others } = current;
          return others;
        });
        update({ ...prefs, widths: { ...prefs.widths, [column.key]: next } });
      },
    };
    const sorting: Partial<ColumnType<T>> = serverSorting
      ? serverSort ? { sorter: true, sortOrder: sort?.key === column.key ? toAntdOrder(sort.order) : null } : {}
      : sortValue ? { sorter: (a: T, b: T) => compareSortValues(sortValue(a), sortValue(b)) } : {};
    return {
      ellipsis: role === "actions" ? false : true,
      ...rest,
      ...sorting,
      title,
      key: column.key,
      width,
      fixed: role === "name" ? "left" : role === "actions" ? "right" : undefined,
      className: [rest.className, role ? `resource-table-${role}-cell` : ""].filter(Boolean).join(" ") || undefined,
      onHeaderCell: (col) => ({ ...(rest.onHeaderCell?.(col) ?? {}), ...(role === "actions" ? {} : resize) }) as ReturnType<NonNullable<ColumnType<T>["onHeaderCell"]>>,
    };
  });

  const expandWidth = typeof expandable?.columnWidth === "number" ? expandable.columnWidth : 48;
  const extraWidth = (rowSelection ? 48 : 0) + (expandable ? expandWidth : 0);
  const scrollX = arranged.reduce((sum, column) => sum + effectiveWidth(column), extraWidth);

  const rows = data ?? [];
  const pageSize = prefs.pageSize ?? 20;
  const setPageSize = (size: number) => {
    if (size !== prefs.pageSize) update({ ...prefs, pageSize: size });
  };
  const showTotal = (total: number) => t("resourceTable.total", { count: total });
  let tablePagination: TablePaginationConfig | false;
  if (pagination === false) {
    tablePagination = false;
  } else if (pagination) {
    tablePagination = {
      showSizeChanger: true,
      pageSizeOptions: PAGE_SIZE_OPTIONS,
      showTotal,
      ...pagination,
      onChange: (page, size) => {
        if (size !== pagination.pageSize) setPageSize(size);
        pagination.onChange?.(page, size);
      },
    };
  } else {
    const pages = Math.max(1, Math.ceil(rows.length / pageSize));
    tablePagination = {
      current: Math.min(clientPage, pages),
      pageSize,
      showSizeChanger: true,
      pageSizeOptions: PAGE_SIZE_OPTIONS,
      showTotal,
      onChange: (page, size) => {
        setClientPage(size !== pageSize ? 1 : page);
        setPageSize(size);
      },
    };
  }

  const onChange: TableProps<T>["onChange"] = (_pagination, filters, sorter, extra) => {
    if (extra.action === "sort" && serverSorting) {
      const single = Array.isArray(sorter) ? sorter[0] : sorter;
      const key = single?.columnKey != null ? String(single.columnKey) : "";
      onSortChange?.(key && single?.order ? { key, order: single.order === "ascend" ? "asc" : "desc" } : null);
    }
    if (extra.action === "filter") {
      if (!pagination) setClientPage(1);
      onFiltersChange?.(Object.fromEntries(Object.entries(filters).map(([key, values]) => [key, (values ?? []).map(String)])));
    }
  };

  const settingsColumns = columns.map((column) => ({
    key: column.key,
    role: column.role,
    label: column.label ?? (typeof column.title === "string" ? column.title : column.role === "actions" ? t("resourceTable.actions") : column.key),
  }));

  if (error) {
    return (
      <div className="resource-table">
        {toolbar && <div className="resource-table-toolbar"><div className="resource-table-toolbar-left">{toolbar}</div></div>}
        <Alert type="error" showIcon message={t("dataTable.loadFailed")} description={error.message} action={onRefresh && <Button icon={<ReloadOutlined />} onClick={onRefresh} aria-label={t("actions.retry")}>{t("actions.retry")}</Button>} />
      </div>
    );
  }
  return (
    <div className="resource-table">
      <div className="resource-table-toolbar">
        <div className="resource-table-toolbar-left">{toolbar}</div>
        <Space className="resource-table-toolbar-right" size={8}>
          {toolbarExtra}
          {updatedAt && <Typography.Text type="secondary" className="resource-table-updated">{t("dataTable.updatedAt", { time: formatTime(updatedAt) })}</Typography.Text>}
          {onRefresh && (
            <Tooltip title={t("dataTable.refresh")}>
              <Button icon={<ReloadOutlined />} onClick={onRefresh} aria-label={t("dataTable.refresh")} />
            </Tooltip>
          )}
          {columnSettings && (
            <Tooltip title={t("resourceTable.settings")}>
              <Button icon={<SettingOutlined />} onClick={() => setSettingsOpen(true)} aria-label={t("resourceTable.settings")} />
            </Tooltip>
          )}
        </Space>
      </div>
      <Table<T>
        rowKey={rowKey}
        columns={antdColumns}
        dataSource={rows}
        loading={loading}
        pagination={tablePagination}
        scroll={{ x: scrollX }}
        tableLayout="fixed"
        components={{ header: { cell: ResizableHeaderCell } }}
        rowSelection={rowSelection ? { fixed: true, columnWidth: 48, ...rowSelection } : undefined}
        expandable={expandable}
        onRow={onRow}
        onChange={onChange}
        showSorterTooltip={false}
        locale={{ emptyText: loading ? <span /> : <EmptyState kind={emptyKind} description={emptyDescription ?? t("dataTable.empty")} action={emptyAction} /> }}
      />
      <ColumnSettingsDrawer
        open={settingsOpen}
        columns={settingsColumns}
        prefs={prefs}
        onChange={update}
        onReset={reset}
        onClose={() => setSettingsOpen(false)}
      />
    </div>
  );
}

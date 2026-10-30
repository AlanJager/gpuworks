import { useEffect, useRef, useState } from "react";
import { Button, Select, Tooltip, Typography, message } from "antd";
import { PlusOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { useSearchParams } from "react-router-dom";
import { useEvents } from "../api";
import { BATCH_POLL_MS, BATCH_STATUSES, batchApi, isBatchActive, type Batch } from "../batchApi";
import { useIsAdmin } from "../components/useIsAdmin";
import PageHeader from "../components/PageHeader";
import ResourceTable, { storedPageSize, type ResourceColumn } from "../components/resourceTable/ResourceTable";
import { formatTime } from "../i18n/datetime";
import { usePagedList } from "../pagedList";
import BatchCreateModal from "./batches/BatchCreateModal";
import BatchAPIUsage from "./batches/BatchAPIUsage";
import BatchDetailDrawer from "./batches/BatchDetailDrawer";
import { batchWarningText } from "./batches/batchErrors";
import { BatchProgress, BatchStatusTag } from "./batches/batchDisplay";
import { useBatchPolling } from "./batches/useBatchPolling";

const TABLE_ID = "batches";

// BatchesPage lists the Batch API jobs, newest first: how far each has got,
// when it was made and when it expires. A row opens the detail drawer. The
// list refreshes itself while any batch on the page is still moving, and
// stops once they have all ended.
// pollMs is the refresh cadence while a batch is moving; only tests change it.
export default function BatchesPage({ pollMs = BATCH_POLL_MS }: { pollMs?: number }) {
  const { t } = useTranslation("batches");
  const isAdmin = useIsAdmin();
  const [searchParams, setSearchParams] = useSearchParams();
  const [status, setStatus] = useState<string[]>([]);
  const [createOpen, setCreateOpen] = useState(false);
  const detailID = searchParams.get("detail");

  const list = usePagedList(batchApi.list, {
    filters: { state: status },
    pageSize: storedPageSize(TABLE_ID, 20),
  });
  const rows = list.data?.items ?? [];
  const refresh = () => { void list.refresh(); };
  useBatchPolling(rows.some((row) => isBatchActive(row.status)), refresh, pollMs);
  // Status changes arrive on the event stream; polling above is the baseline.
  useEvents("batch", refresh);
  // A returning tab should not wait for the next tick.
  const latestRefresh = useRef(refresh);
  latestRefresh.current = refresh;
  useEffect(() => {
    const onVisible = () => { if (!document.hidden) latestRefresh.current(); };
    document.addEventListener("visibilitychange", onVisible);
    return () => document.removeEventListener("visibilitychange", onVisible);
  }, []);

  const openDetail = (id: string) => {
    const next = new URLSearchParams(searchParams);
    next.set("detail", id);
    setSearchParams(next);
  };
  const closeDetail = () => {
    const next = new URLSearchParams(searchParams);
    next.delete("detail");
    setSearchParams(next, { replace: true });
  };
  const created = (batch: Batch) => {
    setCreateOpen(false);
    message.success(t("create.created", { id: batch.id }));
    // A finding that did not refuse the batch, e.g. its model sleeps by an operator's order.
    for (const warning of batch.warnings ?? []) {
      message.warning(batchWarningText(t, warning));
    }
    void list.refetch();
    openDetail(batch.id);
  };

  const columns: ResourceColumn<Batch>[] = [
    {
      key: "id", title: t("columns.id"), width: 240, role: "name",
      render: (_: unknown, batch) => (
        <Button type="link" style={{ padding: 0 }} onClick={() => openDetail(batch.id)} aria-label={t("action.open", { id: batch.id })}>{batch.id}</Button>
      ),
    },
    { key: "status", title: t("columns.status"), width: 130, render: (_: unknown, batch) => <BatchStatusTag status={batch.status} /> },
    { key: "endpoint", title: t("columns.endpoint"), width: 200, render: (_: unknown, batch) => <Typography.Text code>{batch.endpoint}</Typography.Text> },
    {
      key: "model", title: t("columns.model"), width: 180,
      render: (_: unknown, batch) => (batch.model ? <Typography.Text code>{batch.model}</Typography.Text> : <Typography.Text type="secondary">{t("columns.modelPerLine")}</Typography.Text>),
    },
    { key: "progress", title: t("columns.progress"), width: 220, render: (_: unknown, batch) => <BatchProgress batch={batch} /> },
    { key: "created_at", title: t("columns.createdAt"), width: 160, render: (_: unknown, batch) => formatTime(batch.created_at) },
    { key: "expires_at", title: t("columns.expiresAt"), width: 160, render: (_: unknown, batch) => formatTime(batch.expires_at) },
  ];

  const filtering = status.length > 0;
  const toolbar = (
    <Select
      mode="multiple"
      allowClear
      style={{ minWidth: 240 }}
      placeholder={t("filter.status")}
      aria-label={t("filter.status")}
      value={status}
      onChange={setStatus}
      maxTagCount="responsive"
      options={BATCH_STATUSES.map((value) => ({ value, label: t(`status.${value}`) }))}
    />
  );
  const createButton = (
    <Tooltip title={isAdmin ? undefined : t("action.createAdminOnly")}>
      <span>
        <Button type="primary" icon={<PlusOutlined />} disabled={!isAdmin} onClick={() => setCreateOpen(true)}>{t("action.create")}</Button>
      </span>
    </Tooltip>
  );

  return (
    <>
      <PageHeader title={t("page.title")} description={t("page.description")} extra={createButton} />
      <BatchAPIUsage />
      <ResourceTable<Batch>
        tableId={TABLE_ID}
        rowKey="id"
        columns={columns}
        data={rows}
        loading={list.loading}
        error={list.error}
        onRefresh={() => void list.refetch()}
        updatedAt={list.updatedAt}
        pagination={list.pagination}
        toolbar={toolbar}
        emptyDescription={t(filtering ? "empty.filtered" : "empty.all")}
        emptyAction={filtering || !isAdmin ? undefined : <Button type="primary" onClick={() => setCreateOpen(true)}>{t("action.create")}</Button>}
        onRow={(batch) => ({ className: "batch-row", onClick: () => openDetail(batch.id) })}
      />
      <BatchDetailDrawer id={detailID} canManage={isAdmin} pollMs={pollMs} onClose={closeDetail} onChanged={refresh} />
      <BatchCreateModal open={createOpen} onClose={() => setCreateOpen(false)} onCreated={created} />
    </>
  );
}

import { useState } from "react";
import { Alert, Button, Card, Input, Pagination, Space, Spin, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, usePolling, type BoardWaitingWake } from "../api";
import { apiErrorText } from "../apiError";
import EmptyState from "../components/EmptyState";
import PageHeader from "../components/PageHeader";
import { storedPageSize } from "../components/resourceTable/ResourceTable";
import { Legend, NodeList } from "./scheduling/GpuBoard";
import { PendingTable, RecentTable, UpcomingTable, WaitingWakesTable } from "./scheduling/BoardTables";

const BOARD_POLL_MS = 10_000;
const NODES_TABLE_ID = "scheduling-nodes";

// SchedulingPage is the scheduling center: who holds each GPU, what waits to
// be placed or woken, what the schedules do next and what the automation did
// last. The board refreshes itself.
export default function SchedulingPage() {
  const { t } = useTranslation("scheduling");
  const { t: tCommon } = useTranslation("common");
  const [q, setQ] = useState("");
  const [paging, setPaging] = useState({ page: 1, pageSize: storedPageSize(NODES_TABLE_ID, 10), q: "" });
  // A new search starts from the first page.
  const page = paging.q === q ? paging.page : 1;
  const board = usePolling(() => api.schedulingBoard({ page, page_size: paging.pageSize, q }), BOARD_POLL_MS, [page, paging.pageSize, q]);
  const [cancelling, setCancelling] = useState<number | null>(null);
  const data = board.data;

  const cancelWake = async (row: BoardWaitingWake) => {
    setCancelling(row.deployment_id);
    try {
      await api.endWakeWaiting(row.deployment_id);
      message.success(t("waiting.cancelled", { name: row.deployment }));
      await board.refetch();
    } catch (e) {
      message.error(t("waiting.cancelFailed", { detail: apiErrorText(tCommon, e) }));
    } finally {
      setCancelling(null);
    }
  };

  const header = <PageHeader title={t("page.title")} description={t("page.description")} />;
  if (!data && board.error) {
    return (
      <>
        {header}
        <Alert
          type="error"
          showIcon
          message={t("page.loadFailed")}
          description={board.error.message}
          action={<Button onClick={() => void board.refetch()}>{t("page.retry")}</Button>}
        />
      </>
    );
  }
  if (!data) {
    return (
      <>
        {header}
        <div role="status" className="scheduling-loading"><Spin /> <span>{t("page.loading")}</span></div>
      </>
    );
  }

  const nodes = data.nodes;
  return (
    <>
      {header}
      {board.error && (
        <Alert
          style={{ marginBottom: 16 }}
          type="warning"
          showIcon
          message={t("page.refreshFailed", { detail: board.error.message })}
          action={<Button size="small" onClick={() => void board.refetch()}>{t("page.retry")}</Button>}
        />
      )}
      <div className="scheduling-summary" role="group" aria-label={t("summary.title")}>
        <SummaryTile label={t("summary.waitingWakes")} value={data.waiting_wakes.length} />
        <SummaryTile label={t("summary.pending")} value={data.pending.length} />
        <SummaryTile label={t("summary.upcoming")} value={data.upcoming.length} />
      </div>
      <Card className="scheduling-card" title={t("nodes.title")} extra={<Legend />}>
        <Space style={{ marginBottom: 12 }}>
          <Input.Search
            allowClear
            style={{ width: 280 }}
            placeholder={t("nodes.search")}
            aria-label={t("nodes.search")}
            onSearch={(value) => setQ(value.trim())}
          />
        </Space>
        <Spin spinning={board.loading}>
          {nodes.items.length === 0 ? (
            <EmptyState kind="nodes" compact description={t(q ? "nodes.emptyFiltered" : "nodes.empty")} />
          ) : (
            <NodeList nodes={nodes.items} />
          )}
        </Spin>
        {nodes.total > 0 && (
          <Pagination
            className="scheduling-pagination"
            current={page}
            pageSize={paging.pageSize}
            total={nodes.total}
            showSizeChanger
            showTotal={(total) => t("nodes.total", { count: total })}
            onChange={(next, size) => setPaging({ page: size !== paging.pageSize ? 1 : next, pageSize: size, q })}
          />
        )}
      </Card>
      <Card className="scheduling-card" title={t("pending.title")}><PendingTable rows={data.pending} /></Card>
      <Card className="scheduling-card" title={t("waiting.title")}><WaitingWakesTable rows={data.waiting_wakes} busyID={cancelling} onCancel={(row) => void cancelWake(row)} /></Card>
      <Card className="scheduling-card" title={t("upcoming.title")}><UpcomingTable rows={data.upcoming} /></Card>
      <Card className="scheduling-card" title={t("recent.title")}><RecentTable rows={data.recent} /></Card>
    </>
  );
}

function SummaryTile({ label, value }: { label: string; value: number }) {
  return (
    <div className="scheduling-tile">
      <Typography.Text type="secondary">{label}</Typography.Text>
      <strong className="scheduling-num">{value}</strong>
    </div>
  );
}

import { useDeferredValue, useMemo, useRef, useState } from "react";
import { Alert, Button, Card, Spin, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, usePolling } from "../api";
import { schedulingApi, type BoardWaitingWake } from "../schedulingApi";
import { apiErrorText } from "../apiError";
import EmptyState from "../components/EmptyState";
import PageHeader from "../components/PageHeader";
import { PendingTable, UpcomingTable, WaitingWakesTable } from "./scheduling/BoardTables";
import Heatmap from "./scheduling/HeatmapGrid";
import HeatmapLegend from "./scheduling/HeatmapLegend";
import ActivityCard from "./scheduling/ActivityCard";
import HistoryCard from "./scheduling/HistoryCard";
import NodeDrawer from "./scheduling/NodeDrawer";
import { useNodeParam } from "./scheduling/NodeLink";
import OverviewFilters, { DEFAULT_VIEW, type ViewState } from "./scheduling/OverviewFilters";
import SharingStats from "./scheduling/SharingStats";
import SummaryStrip from "./scheduling/SummaryStrip";
import { arrange, buildModel, type NodeView } from "./scheduling/overviewModel";

const POLL_MS = 10_000;

// SchedulingPage is the scheduling center: the whole fleet's GPUs as a heatmap
// with the fleet's summary above it, what waits to be placed or woken, what the
// schedules do next, the stats of the deployments that share cards and, as the
// scheduling history, what the platform and the operators did to the
// deployments. A node opens in a drawer for its cards in detail, from the
// heatmap or any node named on the page. Below the heatmap, the scheduling
// activity over a range and the cards that changed hands most.
export default function SchedulingPage() {
  const { t } = useTranslation("scheduling");
  const { t: tCommon } = useTranslation("common");
  const overview = usePolling(() => schedulingApi.overview(), POLL_MS);
  const queues = usePolling(() => schedulingApi.board({ queues: true }), POLL_MS);
  const [view, setView] = useState<ViewState>(DEFAULT_VIEW);
  // The open node lives in the URL (?node=<uuid>), so a link anywhere on the
  // page opens it, the browser goes back from it, and it can be shared.
  const { node: openUUID, openNode, closeNode } = useNodeParam();
  const [cancelling, setCancelling] = useState<number | null>(null);

  // A poll that finds the fleet unchanged hands the grid the same node views.
  const seen = useRef<Map<string, { signature: string; view: NodeView }>>(undefined);
  const built = useMemo(() => {
    if (!overview.data) return null;
    const result = buildModel(overview.data, seen.current);
    seen.current = result.seen;
    return result.model;
  }, [overview.data]);

  // Typing a deployment name must not hold up the keystroke while thousands of cells restyle.
  const search = useDeferredValue(view.filters.search);
  const arrangement = useMemo(
    () => (built ? arrange(built, { ...view.filters, search }, view.groupBy, view.sortBy) : null),
    [built, view.filters, search, view.groupBy, view.sortBy],
  );
  const openName = useMemo(
    () => (openUUID ? (built?.nodes.find((candidate) => candidate.uuid === openUUID)?.name ?? openUUID) : ""),
    [built, openUUID],
  );

  const cancelWake = async (row: BoardWaitingWake) => {
    setCancelling(row.deployment_id);
    try {
      await api.endWakeWaiting(row.deployment_id);
      message.success(t("waiting.cancelled", { name: row.deployment }));
      await Promise.all([queues.refetch(), overview.refetch()]);
    } catch (e) {
      message.error(t("waiting.cancelFailed", { detail: apiErrorText(tCommon, e) }));
    } finally {
      setCancelling(null);
    }
  };

  const header = <PageHeader title={t("page.title")} description={t("page.description")} />;
  const data = overview.data;
  if (!data && overview.error) {
    return (
      <>
        {header}
        <Alert
          type="error"
          showIcon
          message={t("page.loadFailed")}
          description={overview.error.message}
          action={<Button onClick={() => void overview.refetch()}>{t("page.retry")}</Button>}
        />
      </>
    );
  }
  if (!data || !built || !arrangement) {
    return (
      <>
        {header}
        <div role="status" className="scheduling-loading"><Spin /> <span>{t("page.loading")}</span></div>
      </>
    );
  }

  const board = queues.data;
  const searching = view.filters.search.trim() !== "";
  return (
    <>
      {header}
      {overview.error && (
        <Alert
          style={{ marginBottom: 16 }}
          type="warning"
          showIcon
          message={t("page.refreshFailed", { detail: overview.error.message })}
          action={<Button size="small" onClick={() => void overview.refetch()}>{t("page.retry")}</Button>}
        />
      )}
      <SummaryStrip summary={data.summary} queues={board ? { pending: board.pending.length, upcoming: board.upcoming.length } : undefined} />
      <Card className="scheduling-card" title={t("heatmap.title")} extra={<HeatmapLegend searching={searching} />}>
        {data.summary.cards === 0 ? (
          <EmptyState kind="nodes" compact description={t("nodes.empty")} />
        ) : (
          <>
            <OverviewFilters model={built} view={view} onChange={setView} />
            <p className="hm-status" role="status">
              {t("heatmap.shown", { nodes: arrangement.nodes, cards: arrangement.cards })}
              {searching && ` · ${arrangement.matched.size === 0 ? t("heatmap.noDeployment") : t("heatmap.highlighted", { cards: arrangement.highlighted })}`}
            </p>
            {arrangement.cards === 0 ? (
              <EmptyState kind="nodes" compact description={t("heatmap.emptyFiltered")} />
            ) : (
              <Heatmap arrangement={arrangement} model={built} groupBy={view.groupBy} onOpen={openNode} />
            )}
          </>
        )}
      </Card>
      <ActivityCard />
      <NodeDrawer uuid={openUUID} name={openName} onClose={closeNode} />
      <QueueCards board={board} error={queues.error} onRetry={() => void queues.refetch()} cancelling={cancelling} onCancel={(row) => void cancelWake(row)} />
      <SharingStats />
      <HistoryCard />
    </>
  );
}

function QueueCards({ board, error, onRetry, cancelling, onCancel }: {
  board: Awaited<ReturnType<typeof schedulingApi.board>> | null;
  error: Error | null;
  onRetry: () => void;
  cancelling: number | null;
  onCancel: (row: BoardWaitingWake) => void;
}) {
  const { t } = useTranslation("scheduling");
  if (!board) {
    return error ? (
      <Alert
        style={{ marginBottom: 16 }}
        type="error"
        showIcon
        message={t("queues.loadFailed")}
        description={error.message}
        action={<Button onClick={onRetry}>{t("page.retry")}</Button>}
      />
    ) : (
      <div role="status" className="scheduling-loading"><Spin /> <span>{t("queues.loading")}</span></div>
    );
  }
  return (
    <>
      {error && (
        <Alert
          style={{ marginBottom: 16 }}
          type="warning"
          showIcon
          message={t("page.refreshFailed", { detail: error.message })}
          action={<Button size="small" onClick={onRetry}>{t("page.retry")}</Button>}
        />
      )}
      <Card className="scheduling-card" title={t("pending.title")}><PendingTable rows={board.pending} /></Card>
      <Card className="scheduling-card" title={t("waiting.title")}><WaitingWakesTable rows={board.waiting_wakes} busyID={cancelling} onCancel={onCancel} /></Card>
      <Card className="scheduling-card" title={t("upcoming.title")}><UpcomingTable rows={board.upcoming} /></Card>
    </>
  );
}

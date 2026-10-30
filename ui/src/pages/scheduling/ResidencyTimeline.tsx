import { useState } from "react";
import { Alert, Button, Card, Segmented, Spin, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { usePolling } from "../../api";
import { schedulingApi, timelineWindows, type BoardNode, type TimelineHours } from "../../schedulingApi";
import EmptyState from "../../components/EmptyState";
import { TimelineLegend, TimelineNodes } from "./TimelineLanes";
import { SharingTable } from "./TimelineStats";

// The timeline reads the event log over the window, so it refreshes more
// slowly than the board.
const TIMELINE_POLL_MS = 60_000;

// ResidencyTimeline is the scheduling center's residency timeline for the
// nodes of the board's current page: per card, which deployment held it and in
// what state over the last hours (timelineWindows), and the stats of the deployments
// that share cards.
export default function ResidencyTimeline({ nodes }: { nodes: BoardNode[] }) {
  const { t } = useTranslation("scheduling");
  const [hours, setHours] = useState<TimelineHours>(timelineWindows[0]);
  const uuids = nodes.map((node) => node.uuid);
  const key = uuids.join(",");
  const gpuNames = new Map<string, string>();
  for (const node of nodes) for (const gpu of node.gpus) gpuNames.set(`${node.uuid}/${gpu.index}`, gpu.name);
  const extra = (
    <div className="scheduling-timeline-controls">
      <TimelineLegend />
      <Segmented<TimelineHours>
        aria-label={t("timeline.window")}
        value={hours}
        options={timelineWindows.map((value) => ({ value, label: t("timeline.hours", { hours: value }) }))}
        onChange={setHours}
      />
    </div>
  );
  return (
    <Card className="scheduling-card" title={t("timeline.title")} extra={extra}>
      {uuids.length === 0 ? (
        <EmptyState kind="nodes" compact description={t("timeline.noNodes")} />
      ) : (
        <TimelineBody key={key} uuids={uuids} hours={hours} gpuNames={gpuNames} />
      )}
    </Card>
  );
}

function TimelineBody({ uuids, hours, gpuNames }: { uuids: string[]; hours: TimelineHours; gpuNames: Map<string, string> }) {
  const { t } = useTranslation("scheduling");
  const timeline = usePolling(() => schedulingApi.timeline({ nodes: uuids, hours }), TIMELINE_POLL_MS, [uuids.join(","), hours]);
  const data = timeline.data;
  if (!data && timeline.error) {
    return (
      <Alert
        type="error"
        showIcon
        message={t("timeline.loadFailed")}
        description={timeline.error.message}
        action={<Button onClick={() => void timeline.refetch()}>{t("page.retry")}</Button>}
      />
    );
  }
  if (!data) {
    return <div role="status" className="scheduling-loading"><Spin /> <span>{t("timeline.loading")}</span></div>;
  }
  const cards = data.nodes.reduce((sum, node) => sum + node.gpus.length, 0);
  const held = data.nodes.some((node) => node.gpus.some((gpu) => gpu.segments.length > 0));
  return (
    <Spin spinning={timeline.loading}>
      {timeline.error && (
        <Alert
          style={{ marginBottom: 12 }}
          type="warning"
          showIcon
          message={t("timeline.refreshFailed", { detail: timeline.error.message })}
          action={<Button size="small" onClick={() => void timeline.refetch()}>{t("page.retry")}</Button>}
        />
      )}
      {cards === 0 ? (
        <EmptyState kind="nodes" compact description={t("timeline.noCards")} />
      ) : (
        <>
          {!held && <Typography.Paragraph type="secondary">{t("timeline.noneInWindow", { hours: data.window.hours })}</Typography.Paragraph>}
          <TimelineNodes timeline={data} gpuNames={gpuNames} />
        </>
      )}
      <Typography.Title level={5} className="timeline-sharing-title">{t("timeline.sharingTitle")}</Typography.Title>
      <SharingTable rows={data.deployments} />
    </Spin>
  );
}

import { useEffect, useState } from "react";
import { Alert, Button, Card, Spin, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { usePolling } from "../../api";
import { schedulingApi, type BoardNode, type SchedulingTimeline, type TimelineWindowOption } from "../../schedulingApi";
import EmptyState from "../../components/EmptyState";
import TimeRangePicker from "../../components/TimeRangePicker";
import { formatTime } from "../../i18n/datetime";
import type { TimeRangeSelection } from "../../timeRange";
import { TimelineLegend, TimelineNodes } from "./TimelineLanes";
import { ScopeHint, SharingTable } from "./TimelineStats";
import { askOf, offeredQuickRanges, selectionOfWindow, type TimelineAsk } from "./timelineRange";
import { windowLabel } from "./WindowPicker";

// The timeline reads the event log over the window, so it refreshes more
// slowly than the board. A custom range is fixed and read once.
const TIMELINE_POLL_MS = 60_000;

/** What the server answered about its windows: the ones it lists and the one it answered (none for a custom range). */
interface Offered {
  windows: TimelineWindowOption[];
  key: string;
  seconds: number;
  custom: boolean;
  /** Some card's earlier time is summarized, so the legend explains the hatch. */
  summarized: boolean;
}

// ResidencyTimeline is the scheduling center's residency timeline for the
// nodes of the board's current page: per card, which deployment held it and in
// what state over one of the windows the server lists or a custom range, and
// the stats of the deployments that share cards. A window slides and is
// polled; a custom range is read once.
export default function ResidencyTimeline({ nodes }: { nodes: BoardNode[] }) {
  const { t } = useTranslation("scheduling");
  const [picked, setPicked] = useState<TimeRangeSelection | undefined>(undefined);
  const [offered, setOffered] = useState<Offered | null>(null);
  const uuids = nodes.map((node) => node.uuid);
  const key = uuids.join(",");
  const gpuNames = new Map<string, string>();
  for (const node of nodes) for (const gpu of node.gpus) gpuNames.set(`${node.uuid}/${gpu.index}`, gpu.name);
  // The picker shows what was picked, or the window the server answered.
  const value = picked ?? (offered && !offered.custom ? selectionOfWindow(offered) : null);
  const ask = askOf(picked, offered?.windows ?? []);
  const extra = (
    <div className="scheduling-timeline-controls">
      <TimelineLegend summarized={offered?.summarized ?? false} />
      {offered && value && (
        <TimeRangePicker
          size="small"
          quickRanges={offeredQuickRanges(offered.windows)}
          value={value}
          onChange={(next) => { if (next) setPicked(next); }}
        />
      )}
    </div>
  );
  return (
    <Card className="scheduling-card" title={t("timeline.title")} extra={extra}>
      {uuids.length === 0 ? (
        <EmptyState kind="nodes" compact description={t("timeline.noNodes")} />
      ) : (
        <TimelineBody key={key} uuids={uuids} ask={ask} pollMs={picked?.kind === "custom" ? 0 : TIMELINE_POLL_MS} gpuNames={gpuNames} onAnswer={setOffered} />
      )}
    </Card>
  );
}

function TimelineBody({ uuids, ask, pollMs, gpuNames, onAnswer }: {
  uuids: string[];
  ask: TimelineAsk;
  pollMs: number;
  gpuNames: Map<string, string>;
  onAnswer: (offered: Offered) => void;
}) {
  const { t } = useTranslation("scheduling");
  const timeline = usePolling(() => schedulingApi.timeline({ nodes: uuids, ...ask }), pollMs, [uuids.join(","), JSON.stringify(ask)]);
  const data: SchedulingTimeline | null = timeline.data;
  const summarized = data?.nodes.some((node) => node.gpus.some((gpu) => !!gpu.summary?.length)) ?? false;
  const offeredKey = data ? `${data.window.key}|${data.window.seconds}|${data.windows.map((w) => w.key).join(",")}|${summarized}` : "";
  useEffect(() => {
    if (data) onAnswer({ windows: data.windows, key: data.window.key, seconds: data.window.seconds, custom: data.window.custom, summarized });
    // The answer's windows change rarely; offeredKey says when they did.
  }, [offeredKey]);
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
  const held = data.nodes.some((node) => node.gpus.some((gpu) => gpu.segments.length > 0 || !!gpu.summary?.length));
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
          {!held && <Typography.Paragraph type="secondary">{emptyText(t, data)}</Typography.Paragraph>}
          <TimelineNodes timeline={data} gpuNames={gpuNames} />
        </>
      )}
      <Typography.Title level={5} className="timeline-sharing-title">{t("timeline.sharingTitle")}<ScopeHint scope={data.scope} /></Typography.Title>
      {!data.switches_available && <Typography.Paragraph type="secondary">{t("timeline.switchesUnavailable")}</Typography.Paragraph>}
      <SharingTable rows={data.deployments} switchesAvailable={data.switches_available} />
    </Spin>
  );
}

// emptyText says nobody held the cards, over the window by its length or over
// a custom range by its ends.
function emptyText(t: TFunction<"scheduling">, data: SchedulingTimeline): string {
  if (data.window.custom) return t("timeline.noneInRange", { from: formatTime(data.window.from), to: formatTime(data.window.to) });
  return t("timeline.noneInWindow", { span: windowLabel(t, data.window) });
}

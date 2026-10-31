import { useMemo, useState } from "react";
import { Alert, Button, Card, Spin, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { usePolling } from "../../api";
import { schedulingApi, type ActivityCard as TopCard, type SchedulingActivity } from "../../schedulingApi";
import EmptyState from "../../components/EmptyState";
import TimeRangePicker from "../../components/TimeRangePicker";
import { SCHEDULING_QUICK_RANGES, resolveRange, type TimeRangeSelection } from "../../timeRange";
import EChart from "../analytics/EChart";
import { activityOption, isEmpty } from "./activityChart";
import { NodeLink } from "./NodeLink";

// The chart's height; the list of busiest cards beside it shows as many rows
// as fit in it.
export const ACTIVITY_CHART_HEIGHT = 240;
const TOP_ROW_HEIGHT = 28;
const TOP_HEADER_HEIGHT = 32;
export const TOP_ROWS_SHOWN = Math.floor((ACTIVITY_CHART_HEIGHT - TOP_HEADER_HEIGHT) / TOP_ROW_HEIGHT);
const DEFAULT_RANGE: TimeRangeSelection = { kind: "quick", key: "1d" };

/**
 * ActivityCard is the scheduling activity over a range (the last 24 hours
 * until another is picked): the scheduling events per bucket stacked by
 * reason, and beside them the cards that changed hands most, each opening
 * its node's drawer. The bucket, the reasons and the cards are the
 * server's; a quick range is polled, a custom one read once.
 */
export default function ActivityCard() {
  const { t } = useTranslation("scheduling");
  const [range, setRange] = useState<TimeRangeSelection>(DEFAULT_RANGE);
  const pollMs = resolveRange(range, Date.now()).pollMs;
  // A quick range slides and is polled; a custom one is read once (pollMs 0).
  const activity = usePolling(() => schedulingApi.activity(range), pollMs, [JSON.stringify(range)]);
  const data = activity.data;
  const picker = (
    <TimeRangePicker size="small" quickRanges={SCHEDULING_QUICK_RANGES} value={range} onChange={(next) => { if (next) setRange(next); }} />
  );
  let body;
  if (!data && activity.error) {
    body = (
      <Alert type="error" showIcon message={t("activity.loadFailed")} description={activity.error.message}
        action={<Button onClick={() => void activity.refetch()}>{t("page.retry")}</Button>} />
    );
  } else if (!data) {
    body = <div role="status" className="scheduling-loading"><Spin /> <span>{t("activity.loading")}</span></div>;
  } else {
    body = (
      <Spin spinning={activity.loading}>
        {activity.error && (
          <Alert style={{ marginBottom: 12 }} type="warning" showIcon message={t("activity.refreshFailed", { detail: activity.error.message })}
            action={<Button size="small" onClick={() => void activity.refetch()}>{t("page.retry")}</Button>} />
        )}
        {data.beyond_retention && <Alert style={{ marginBottom: 12 }} type="info" showIcon message={t("activity.beyondRetention")} />}
        <ActivityBody data={data} />
      </Spin>
    );
  }
  return <Card className="scheduling-card" title={t("activity.title")} extra={picker}>{body}</Card>;
}

function ActivityBody({ data }: { data: SchedulingActivity }) {
  const { t, i18n } = useTranslation("scheduling");
  const option = useMemo(() => activityOption(data, (key) => t(`activity.reason.${key}`, { defaultValue: key }), i18n.language), [data, t, i18n.language]);
  const bucket = bucketText(t, data.bucket_seconds);
  return (
    <div className="activity-body">
      <div className="activity-chart">
        {isEmpty(data) ? (
          <div style={{ height: ACTIVITY_CHART_HEIGHT }} className="activity-empty"><EmptyState compact description={t("activity.empty")} /></div>
        ) : (
          <EChart option={option} height={ACTIVITY_CHART_HEIGHT} label={t("activity.chartAria", { bucket })} />
        )}
        <Typography.Text type="secondary" className="activity-totals">
          {t("activity.totals", { bucket })}
          {data.reasons.filter((reason) => reason.count > 0).map((reason) => (
            <span key={reason.key} className="activity-total">{t(`activity.reason.${reason.key}`, { defaultValue: reason.key })} {reason.count}</span>
          ))}
        </Typography.Text>
      </div>
      <TopCards cards={data.top_cards} />
    </div>
  );
}

function TopCards({ cards }: { cards: TopCard[] }) {
  const { t } = useTranslation("scheduling");
  const shown = cards.slice(0, TOP_ROWS_SHOWN);
  return (
    <div className="activity-top" style={{ height: ACTIVITY_CHART_HEIGHT }}>
      <Typography.Text strong className="activity-top-title">{t("activity.topCards")}</Typography.Text>
      {shown.length === 0 ? (
        <Typography.Paragraph type="secondary">{t("activity.topCardsEmpty")}</Typography.Paragraph>
      ) : (
        <ol className="activity-top-list" aria-label={t("activity.topCards")}>
          {shown.map((card) => (
            <li key={`${card.node_uuid}/${card.gpu_index}`} style={{ height: TOP_ROW_HEIGHT }}>
              <span className="activity-top-card">
                <NodeLink uuid={card.node_uuid} name={card.node || card.node_uuid} />
                <span> · {t("activity.gpu", { index: card.gpu_index })}</span>
              </span>
              <span className="activity-top-count">{t("activity.switches", { count: card.switches })}</span>
            </li>
          ))}
        </ol>
      )}
    </div>
  );
}

function bucketText(t: ReturnType<typeof useTranslation>["t"], seconds: number): string {
  if (seconds % 86_400 === 0) return t("activity.bucket.days", { value: seconds / 86_400 });
  if (seconds % 3600 === 0) return t("activity.bucket.hours", { value: seconds / 3600 });
  if (seconds % 60 === 0) return t("activity.bucket.minutes", { value: seconds / 60 });
  return t("activity.bucket.seconds", { value: seconds });
}

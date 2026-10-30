import { useState } from "react";
import { Alert, Button, Card, Segmented, Spin } from "antd";
import { useTranslation } from "react-i18next";
import { api, timelineWindows, usePolling, type TimelineHours } from "../../api";
import { SharingTable } from "./TimelineStats";

const SHARING_POLL_MS = 60_000;

/**
 * SharingStats is the stats of the deployments that share cards (every
 * yield policy but never) across the whole fleet: how long each was awake,
 * how often it changed places with others and how long the requests held for
 * its wake waited. The timeline lanes are the node drawer's; this is the only
 * read of the event log the page makes for the fleet, and only for those
 * deployments.
 */
export default function SharingStats() {
  const { t } = useTranslation("scheduling");
  const [hours, setHours] = useState<TimelineHours>(timelineWindows[0]);
  const stats = usePolling(() => api.schedulingTimeline({ sharing: true, hours }), SHARING_POLL_MS, [hours]);
  const data = stats.data;
  const window = (
    <Segmented<TimelineHours>
      aria-label={t("timeline.window")}
      value={hours}
      options={timelineWindows.map((value) => ({ value, label: t("timeline.hours", { hours: value }) }))}
      onChange={setHours}
    />
  );
  let body;
  if (!data && stats.error) {
    body = (
      <Alert type="error" showIcon message={t("sharing.loadFailed")} description={stats.error.message} action={<Button onClick={() => void stats.refetch()}>{t("page.retry")}</Button>} />
    );
  } else if (!data) {
    body = <div role="status" className="scheduling-loading"><Spin /> <span>{t("sharing.loading")}</span></div>;
  } else {
    body = (
      <Spin spinning={stats.loading}>
        {stats.error && (
          <Alert
            style={{ marginBottom: 12 }}
            type="warning"
            showIcon
            message={t("sharing.refreshFailed", { detail: stats.error.message })}
            action={<Button size="small" onClick={() => void stats.refetch()}>{t("page.retry")}</Button>}
          />
        )}
        <SharingTable rows={data.deployments} />
      </Spin>
    );
  }
  return <Card className="scheduling-card" title={t("sharing.title")} extra={window}>{body}</Card>;
}

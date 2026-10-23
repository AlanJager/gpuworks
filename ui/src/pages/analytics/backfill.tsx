import { Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { AnalyticsRange } from "../../analyticsApi";

/**
 * leadingBackfilled is how many leading buckets began before live accumulation did: those hold figures filled in
 * from the request log. Only when there is such history (the statistic's oldest bucket is older than live
 * accumulation); a fresh installation has nothing filled in, so nothing is shaded.
 */
export function leadingBackfilled(range: AnalyticsRange): number {
  // The bucket live accumulation began in starts before it began, so history
  // means a bucket at least one whole bucket older.
  const bucket = range.granularity === "day" ? 24 * 3_600_000 : 3_600_000;
  if (!range.live_since_ms || !range.earliest_ms || range.earliest_ms + bucket > range.live_since_ms) return 0;
  return range.buckets.filter((b) => b < range.live_since_ms).length;
}

// BackfillNote explains the shaded buckets; it is shown only when the range
// starts before live accumulation did and there is filled-in history.
export default function BackfillNote({ range, kind }: { range: AnalyticsRange; kind: "usage" | "quality" }) {
  const { t } = useTranslation("analytics");
  if (leadingBackfilled(range) === 0) return null;
  return <Typography.Paragraph type="secondary" style={{ fontSize: 12, margin: "4px 0 0" }}>{t(`backfill.${kind}`)}</Typography.Paragraph>;
}

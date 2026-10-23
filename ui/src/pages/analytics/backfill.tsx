import { Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { AnalyticsRange } from "../../analyticsApi";

/** leadingBackfilled is how many leading buckets began before live accumulation did: those hold figures filled in from the request log. */
export function leadingBackfilled(range: AnalyticsRange): number {
  if (!range.live_since_ms) return 0;
  return range.buckets.filter((b) => b < range.live_since_ms).length;
}

// BackfillNote explains the shaded buckets; it is shown only when the range
// starts before live accumulation did.
export default function BackfillNote({ range, kind }: { range: AnalyticsRange; kind: "usage" | "quality" }) {
  const { t } = useTranslation("analytics");
  if (leadingBackfilled(range) === 0) return null;
  return <Typography.Paragraph type="secondary" style={{ fontSize: 12, margin: "4px 0 0" }}>{t(`backfill.${kind}`)}</Typography.Paragraph>;
}

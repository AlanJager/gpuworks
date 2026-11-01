import { Tooltip, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { LogsBucket } from "../api";
import { formatTimeSec } from "../i18n/datetime";

// LogsHistogram draws log volume per time bucket, error/fatal stacked in red
// over everything else in blue. With onPick each bar is a button that hands
// back its bucket start, so the page can narrow its window to it.
export default function LogsHistogram({ buckets, onPick }: { buckets: LogsBucket[]; onPick?: (startMS: number) => void }) {
  const { t } = useTranslation("logs");
  if (!buckets.length) return null;
  const oldestFirst = [...buckets].sort((a, b) => a.start_ms - b.start_ms);
  // Lines without a parsed level (old agents predate classification) count
  // toward the blue volume: they are real traffic, just unstamped.
  const levelTotal = (b: LogsBucket, keys: string[]) => keys.reduce((sum, k) => sum + (b.counts[k] ?? 0), 0);
  const totals = oldestFirst.map((b) => {
    const known = levelTotal(b, ["info", "warn", "debug", "error", "fatal"]);
    const unknown = Object.entries(b.counts).reduce((sum, [k, n]) => (["info", "warn", "debug", "error", "fatal"].includes(k) ? sum : sum + n), 0);
    return known + unknown;
  });
  const max = Math.max(...totals, 1);
  return (
    <div style={{ marginBottom: 12 }} data-testid="logs-histogram">
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        {t("histogram.caption")}
      </Typography.Text>
      <div style={{ display: "flex", alignItems: "flex-end", gap: 2, height: 64, marginTop: 6 }}>
        {oldestFirst.map((b, i) => {
          const bad = (b.counts["error"] ?? 0) + (b.counts["fatal"] ?? 0); // unstamped lines are not "bad"
          const good = totals[i] - bad;
          return (
            <Tooltip key={b.start_ms} title={`${formatTimeSec(b.start_ms)} · ${totals[i]}`}>
              <div
                role={onPick ? "button" : "img"}
                tabIndex={onPick ? 0 : undefined}
                aria-label={`${formatTimeSec(b.start_ms)} ${totals[i]}`}
                onClick={() => onPick?.(b.start_ms)}
                onKeyDown={(e) => {
                  if (onPick && (e.key === "Enter" || e.key === " ")) {
                    e.preventDefault();
                    onPick(b.start_ms);
                  }
                }}
                style={{ flex: 1, height: `${Math.max((totals[i] / max) * 100, 3)}%`, display: "flex", flexDirection: "column", justifyContent: "flex-end", cursor: onPick ? "pointer" : "default", minWidth: 3 }}
              >
                <div style={{ background: "#cf1322", height: totals[i] ? `${(bad / totals[i]) * 100}%` : 0, borderRadius: 2 }} />
                <div style={{ background: "#5b8ff0", height: totals[i] ? `${(good / totals[i]) * 100}%` : 0, borderRadius: 2 }} />
              </div>
            </Tooltip>
          );
        })}
      </div>
    </div>
  );
}

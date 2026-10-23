import { Typography } from "antd";
import { DASH, fmtPct } from "./format";

// RateBar is a percentage with a small bar; null is a dash.
export default function RateBar({ percent, color = "#0076f7" }: { percent: number | null; color?: string }) {
  if (percent == null) return <Typography.Text type="secondary">{DASH}</Typography.Text>;
  return (
    <span style={{ display: "inline-flex", alignItems: "center", gap: 8, justifyContent: "flex-end" }}>
      <span aria-hidden="true" style={{ width: 56, height: 6, background: "#f0f2f5", borderRadius: 3, overflow: "hidden", flex: "none" }}>
        <span style={{ display: "block", height: "100%", width: `${Math.min(100, Math.max(0, percent))}%`, background: color, borderRadius: 3 }} />
      </span>
      <span style={{ minWidth: 46, textAlign: "right" }}>{fmtPct(percent)}</span>
    </span>
  );
}

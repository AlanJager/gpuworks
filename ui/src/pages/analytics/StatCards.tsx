import type { ReactNode } from "react";
import { Card, Tooltip, Typography } from "antd";
import { InfoCircleOutlined } from "@ant-design/icons";

export interface StatItem {
  key: string;
  title: string;
  value: string;
  /** The exact figure, shown as the value's title when value is abbreviated. */
  exact?: string;
  sub?: ReactNode;
  /** A swatch matching the series this figure is in the chart. */
  dot?: string;
  /** What the figure means, behind an info icon. */
  tip?: string;
}

// StatCards is the row of summary figures above a tab's chart.
export default function StatCards({ items }: { items: StatItem[] }) {
  return (
    <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(170px, 1fr))", gap: 12, marginBottom: 16 }}>
      {items.map((item) => (
        <Card key={item.key} size="small" data-stat={item.key}>
          <div style={{ display: "flex", alignItems: "center", gap: 6, whiteSpace: "nowrap" }}>
            {item.dot && <span aria-hidden="true" style={{ display: "inline-block", width: 8, height: 8, borderRadius: 2, background: item.dot }} />}
            <Typography.Text type="secondary">{item.title}</Typography.Text>
            {item.tip && (
              <Tooltip title={item.tip}>
                <InfoCircleOutlined tabIndex={0} role="img" aria-label={item.tip} style={{ color: "#9ca3af" }} />
              </Tooltip>
            )}
          </div>
          <div title={item.exact} style={{ fontSize: 24, fontWeight: 600, lineHeight: 1.4, fontVariantNumeric: "tabular-nums", whiteSpace: "nowrap" }}>{item.value}</div>
          {item.sub != null && <Typography.Text type="secondary" style={{ fontSize: 12 }}>{item.sub}</Typography.Text>}
        </Card>
      ))}
    </div>
  );
}

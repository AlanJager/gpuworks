import type { ReactNode } from "react";
import { Typography } from "antd";

// Shared "not collected"/null placeholder for table cells across the monitor
// pages: a muted dash, never a fabricated 0 (wire contract: "Null means not
// collected... the UI renders a muted dash, never 0").
export default function Dash() {
  return <Typography.Text type="secondary">—</Typography.Text>;
}

// renderNumber renders one side of a pair of independently-nullable numbers
// (e.g. running/waiting). Each side must fall back to its own dash: when
// only one side is null, "38 / 0" fabricates a value the server never sent.
export function renderNumber(value: number | null | undefined, format: (v: number) => string = (v) => String(v)): ReactNode {
  return value == null ? <Dash /> : format(value);
}

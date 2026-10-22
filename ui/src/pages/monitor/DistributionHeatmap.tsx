import { useMemo, useState, type KeyboardEvent } from "react";
import { Typography } from "antd";
import type { MonitorGridCell } from "../../monitorRequestsApi";
import { colors } from "../../theme/tokens";
import { gridView, shareOf } from "./requestShape";

// One hue, light to dark, for request counts (log scaled: a distribution's
// tail is a handful of requests beside thousands).
const RAMP = ["#ceefff", "#9bd9ff", "#5db8ff", "#0076f7", "#005bd4", "#052e82"];

export function rampColor(n: number, max: number): string {
  if (n <= 0 || max <= 0) return colors.borderSoft;
  const t = Math.log1p(n) / Math.log1p(max);
  return RAMP[Math.min(RAMP.length - 1, Math.floor(t * RAMP.length))];
}

interface Props {
  cells: MonitorGridCell[];
  height: number;
  xTitle: string;
  yTitle: string;
  // Short axis tick for a bucket, and the full range for the readout.
  xTick: (i: number) => string;
  yTick: (i: number) => string;
  xRange: (i: number) => string;
  yRange: (i: number) => string;
  // readout words a hovered cell: "1K–2K in · 128–256 out: 31 requests (4%)".
  readout: (x: string, y: string, n: number, share: string) => string;
  hint: string;
  empty: string;
  label: string;
}

// DistributionHeatmap draws request counts over two bucketed axes, trimmed to
// the buckets that hold requests. Hovering a cell, or moving over the grid
// with the arrow keys once it has focus, names the cell in the readout line
// below - one line instead of a tooltip per cell.
export default function DistributionHeatmap(props: Props) {
  const { cells, height } = props;
  const view = useMemo(() => gridView(cells), [cells]);
  const [cursor, setCursor] = useState<{ x: number; y: number } | null>(null);

  if (view.total === 0) {
    return (
      <Typography.Text type="secondary" style={{ height, display: "flex", alignItems: "center", justifyContent: "center", textAlign: "center" }}>
        {props.empty}
      </Typography.Text>
    );
  }

  // Rows top to bottom from the largest bucket, so "more" is up.
  const ys = [...view.ys].reverse();
  const every = (count: number) => (count > 10 ? 3 : count > 6 ? 2 : 1);
  const xEvery = every(view.xs.length);
  const yEvery = every(ys.length);
  const active = cursor ?? null;
  const activeN = active ? (view.counts.get(`${active.x}:${active.y}`) ?? 0) : 0;
  const readout = active
    ? props.readout(props.xRange(active.x), props.yRange(active.y), activeN, shareOf(activeN, view.total))
    : props.hint;

  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    const moves: Record<string, [number, number]> = { ArrowLeft: [-1, 0], ArrowRight: [1, 0], ArrowUp: [0, 1], ArrowDown: [0, -1] };
    const move = moves[event.key];
    if (!move) return;
    event.preventDefault();
    const from = cursor ?? { x: view.xs[0], y: view.ys[0] };
    const clamp = (v: number, list: number[]) => Math.min(list[list.length - 1], Math.max(list[0], v));
    setCursor(cursor ? { x: clamp(from.x + move[0], view.xs), y: clamp(from.y + move[1], view.ys) } : from);
  };

  const tickStyle = { fontSize: 11, color: colors.muted, fontVariantNumeric: "tabular-nums" as const, whiteSpace: "nowrap" as const };
  return (
    <div style={{ height, display: "flex", flexDirection: "column", gap: 4 }}>
      <div style={{ display: "flex", flex: 1, minHeight: 0, gap: 4 }}>
        <div style={{ display: "grid", gridTemplateRows: `repeat(${ys.length}, 1fr)`, textAlign: "right" }} aria-hidden>
          {ys.map((y, i) => (
            <span key={y} style={{ ...tickStyle, lineHeight: 1, alignSelf: "center" }}>
              {i % yEvery === 0 ? props.yTick(y) : ""}
            </span>
          ))}
        </div>
        <div
          role="grid"
          aria-label={props.label}
          tabIndex={0}
          onKeyDown={onKeyDown}
          onBlur={() => setCursor(null)}
          onMouseLeave={() => setCursor(null)}
          style={{
            flex: 1,
            display: "grid",
            gridTemplateColumns: `repeat(${view.xs.length}, 1fr)`,
            gridTemplateRows: `repeat(${ys.length}, 1fr)`,
            gap: 1,
            outlineOffset: 2,
          }}
        >
          {ys.map((y) =>
            view.xs.map((x) => {
              const n = view.counts.get(`${x}:${y}`) ?? 0;
              const selected = active?.x === x && active?.y === y;
              return (
                <div
                  key={`${x}:${y}`}
                  role="gridcell"
                  aria-label={props.readout(props.xRange(x), props.yRange(y), n, shareOf(n, view.total))}
                  data-count={n}
                  onMouseEnter={() => setCursor({ x, y })}
                  style={{
                    background: rampColor(n, view.max),
                    borderRadius: 1,
                    boxShadow: selected ? `inset 0 0 0 2px ${colors.ink}` : undefined,
                  }}
                />
              );
            }),
          )}
        </div>
      </div>
      <div style={{ display: "flex", gap: 4 }} aria-hidden>
        <span style={{ ...tickStyle, visibility: "hidden" }}>{props.yTick(ys[0])}</span>
        <div style={{ flex: 1, display: "grid", gridTemplateColumns: `repeat(${view.xs.length}, 1fr)` }}>
          {view.xs.map((x, i) => (
            <span key={x} style={{ ...tickStyle, overflow: "visible" }}>
              {i % xEvery === 0 ? props.xTick(x) : ""}
            </span>
          ))}
        </div>
      </div>
      <div style={{ display: "flex", justifyContent: "space-between", gap: 8, fontSize: 11, color: colors.muted }}>
        <span>
          ↑ {props.yTitle} · → {props.xTitle}
        </span>
      </div>
      <Typography.Text style={{ fontSize: 12, minHeight: 18 }} type={active ? undefined : "secondary"} aria-live="polite">
        {readout}
      </Typography.Text>
    </div>
  );
}

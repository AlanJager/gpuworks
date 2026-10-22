import { Space, Tag, Typography } from "antd";
import type { GPU, Topology, TopoHighlight } from "../api";
import { colors } from "../theme/tokens";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";

const CARD_W = 120;
const CARD_H = 66;
const CARD_GAP_X = 14;
const CARD_GAP_Y = 12;
const BAND_PAD_X = 18;
const BAND_PAD_TOP = 18;
const BAND_PAD_BOTTOM = 18;
const BAND_GAP = 18;
const HEADER_H = 24;
const MAX_COLS = 4;
const LEFT_GUTTER = 16;

interface Pos {
  x: number;
  y: number;
  cx: number;
  cy: number;
  groupKey: string;
}

interface Group {
  key: string;
  numa: number;
  gpus: number[];
}

interface GroupLayout {
  group: Group;
  x: number;
  y: number;
  w: number;
  h: number;
  cols: number;
  rows: number;
}

function buildGroups(topo: Topology): Group[] {
  const assigned = new Set<number>();
  const groups: Group[] = [];
  const entries = Object.entries(topo.numa_groups ?? {});
  for (const [key, idxs] of entries) {
    const numa = Number(key);
    if (key === "-1" || key.toLowerCase() === "unknown" || Number.isNaN(numa)) continue;
    const g = [...idxs].sort((a, b) => a - b);
    g.forEach((i) => assigned.add(i));
    groups.push({ key, numa, gpus: g });
  }
  groups.sort((a, b) => a.numa - b.numa);

  const unknown: number[] = [];
  for (const g of topo.gpus ?? []) {
    if (!assigned.has(g.index)) unknown.push(g.index);
  }
  unknown.sort((a, b) => a - b);
  if (unknown.length) groups.push({ key: "unknown", numa: -1, gpus: unknown });
  return groups;
}

function classifyLinks(topo: Topology) {
  const matrix = topo.matrix ?? [];
  let maxClass = 0;
  for (let i = 0; i < matrix.length; i++) {
    for (let j = 0; j < (matrix[i]?.length ?? 0); j++) {
      if (i === j) continue;
      const c = matrix[i][j]?.class ?? 0;
      if (c > maxClass) maxClass = c;
    }
  }

  let maxClassIsFast = false;
  for (let i = 0; i < matrix.length; i++) {
    for (let j = 0; j < (matrix[i]?.length ?? 0); j++) {
      if (i === j) continue;
      const l = matrix[i][j];
      if (!l || l.class !== maxClass) continue;
      if (isFastRaw(l)) maxClassIsFast = true;
    }
  }

  const isFast = (a: number, b: number): boolean => {
    const l = matrix[a]?.[b];
    if (!l) return false;
    return isFastRaw(l) || (maxClassIsFast && l.class === maxClass);
  };

  return { matrix, isFast };
}

function isFastRaw(l: { raw?: string; nvlink_bonds?: number }): boolean {
  const raw = (l.raw ?? "").toUpperCase();
  return /^NV/.test(raw) || /^HCCS/.test(raw) || /^XGMI/.test(raw) || /^ICN/.test(raw) || (l.nvlink_bonds ?? 0) > 0;
}

function fastLabel(l: { raw?: string; nvlink_bonds?: number }): string {
  const raw = (l.raw ?? "").toUpperCase();
  if (/^ICN/.test(raw)) return raw;
  if ((l.nvlink_bonds ?? 0) > 0) return `NV${l.nvlink_bonds}`;
  if (/^HCCS/.test(raw)) return "HCCS";
  if (/^XGMI/.test(raw)) return "XGMI";
  if (/^NV/.test(raw)) return "NV";
  return "FAST";
}

function shortText(v: string, max = 14): string {
  return v.length > max ? `${v.slice(0, max - 1)}…` : v;
}

function gpuMeta(gpu: GPU | undefined, t: TFunction<"nodes">): string {
  if (!gpu) return t("topology.accelerator");
  const vendor = gpu.vendor ? `${gpu.vendor} · ` : "";
  const arch = gpu.arch_id || gpu.arch_family || "unknown";
  return `${vendor}${shortText(arch, 16)}`;
}

function laneTitle(group: Group, t: TFunction<"nodes">): string {
  return group.numa < 0 ? t("topology.numaUnknown") : `NUMA ${group.numa}`;
}

function laneSubtitle(group: Group, t: TFunction<"nodes">): string {
  return t("topology.acceleratorCount", { count: group.gpus.length });
}

function cliqueBounds(clique: number[], pos: Record<number, Pos>) {
  const points = clique.map((idx) => pos[idx]).filter((p): p is Pos => !!p);
  if (points.length === 0) return null;
  const minX = Math.min(...points.map((p) => p.x));
  const minY = Math.min(...points.map((p) => p.y));
  const maxX = Math.max(...points.map((p) => p.x + CARD_W));
  const maxY = Math.max(...points.map((p) => p.y + CARD_H));
  return { x: minX - 8, y: minY - 8, w: maxX - minX + 16, h: maxY - minY + 16 };
}

function linkPath(a: Pos, b: Pos) {
  const mx = (a.cx + b.cx) / 2;
  const my = (a.cy + b.cy) / 2;
  const dx = Math.abs(a.cx - b.cx);
  const dy = Math.abs(a.cy - b.cy);
  const lift = Math.max(24, Math.min(74, dx * 0.16 + dy * 0.35));
  const bend = a.cy === b.cy ? lift : lift * 0.6;
  const controlY = my - bend;
  return `M ${a.cx} ${a.cy} Q ${mx} ${controlY} ${b.cx} ${b.cy}`;
}

function edgeLabelPos(a: Pos, b: Pos) {
  return {
    x: (a.cx + b.cx) / 2,
    y: (a.cy + b.cy) / 2 - 10,
  };
}

export default function TopologyGraph({
  topology,
  highlight,
  gpus,
}: {
  topology?: Topology;
  highlight?: TopoHighlight;
  gpus?: GPU[];
}) {
  const { t } = useTranslation("nodes");
  if (!topology || !(topology.gpus?.length)) {
    return (
      <Typography.Text type="secondary" style={{ display: "block", padding: "16px 0" }}>
        {t("topology.empty")}
      </Typography.Text>
    );
  }

  const groups = buildGroups(topology);
  const { matrix, isFast } = classifyLinks(topology);
  const gpuMap = new Map<number, GPU>();
  for (const gpu of gpus ?? []) gpuMap.set(gpu.index, gpu);

  const hlGpus = new Set(highlight?.gpus ?? []);
  const hlNuma = new Set(highlight?.numa ?? []);
  const hlLinks = new Set((highlight?.links ?? []).map(([a, b]) => (a < b ? `${a}-${b}` : `${b}-${a}`)));
  const isHlLink = (a: number, b: number) => hlLinks.has(a < b ? `${a}-${b}` : `${b}-${a}`);

  const positions: Record<number, Pos> = {};
  const layouts: GroupLayout[] = [];
  let maxW = 0;
  let curY = 12;

  for (const group of groups) {
    const count = group.gpus.length;
    const cols = Math.min(MAX_COLS, Math.max(1, count));
    const rows = Math.ceil(count / cols);
    const innerW = cols * CARD_W + Math.max(0, cols - 1) * CARD_GAP_X;
    const innerH = rows * CARD_H + Math.max(0, rows - 1) * CARD_GAP_Y;
    const w = innerW + BAND_PAD_X * 2;
    const h = HEADER_H + BAND_PAD_TOP + innerH + BAND_PAD_BOTTOM;
    const x = LEFT_GUTTER;
    const y = curY;

    group.gpus.forEach((idx, order) => {
      const col = order % cols;
      const row = Math.floor(order / cols);
      const px = x + BAND_PAD_X + col * (CARD_W + CARD_GAP_X);
      const py = y + HEADER_H + BAND_PAD_TOP + row * (CARD_H + CARD_GAP_Y);
      positions[idx] = {
        x: px,
        y: py,
        cx: px + CARD_W / 2,
        cy: py + CARD_H / 2,
        groupKey: group.key,
      };
    });

    layouts.push({ group, x, y, w, h, cols, rows });
    maxW = Math.max(maxW, x + w);
    curY = y + h + BAND_GAP;
  }

  const vbW = maxW + LEFT_GUTTER;
  const vbH = curY + 10;
  const cliques = (topology.fast_cliques?.length ? topology.fast_cliques : topology.nvlink_cliques) ?? [];

  const edges: JSX.Element[] = [];
  const labels: JSX.Element[] = [];
  const idxs = Object.keys(positions).map(Number).sort((a, b) => a - b);

  for (let a = 0; a < idxs.length; a++) {
    for (let b = a + 1; b < idxs.length; b++) {
      const i = idxs[a];
      const j = idxs[b];
      const link = matrix[i]?.[j];
      if (!link) continue;
      const raw = (link.raw ?? "").toUpperCase();
      const fast = isFast(i, j);
      const isSys = raw === "SYS";
      if (!fast && (isSys || raw === "" || raw === "X")) continue;

      const hl = isHlLink(i, j);
      const p1 = positions[i];
      const p2 = positions[j];
      const stroke = hl ? colors.chartPrimary : fast ? colors.success : colors.border;
      const width = hl ? (fast ? 4.5 : 3.25) : fast ? 3.25 : 1.5;
      const dash = fast ? undefined : "4 4";
      edges.push(
        <path
          key={`e-${i}-${j}`}
          d={linkPath(p1, p2)}
          fill="none"
          stroke={stroke}
          strokeWidth={width}
          strokeLinecap="round"
          strokeDasharray={dash}
          opacity={hl ? 0.98 : fast ? 0.9 : 0.7}
        />,
      );

      if (fast) {
        const { x, y } = edgeLabelPos(p1, p2);
        labels.push(
          <text
            key={`l-${i}-${j}`}
            x={x}
            y={y}
            textAnchor="middle"
            fontSize={10}
            fontWeight={600}
            fill={hl ? colors.chartPrimary : colors.successText}
            style={{ paintOrder: "stroke", stroke: colors.white, strokeWidth: 3 }}
          >
            {fastLabel(link)}
          </text>,
        );
      }
    }
  }

  return (
    <div
      style={{
        border: `1px solid ${colors.borderSoft}`,
        borderRadius: 4,
        background: colors.white,
        overflowX: "auto",
        padding: "14px 14px 8px",
      }}
    >
      <div style={{ display: "flex", justifyContent: "space-between", gap: 12, marginBottom: 10, flexWrap: "wrap" }}>
        <Space size={8} wrap>
          <Tag color="blue">GPU {topology.gpus.length}</Tag>
          <Tag color="geekblue">NUMA {groups.length}</Tag>
          <Tag color="green">{t("topology.fastLinkCount", { count: countFastLinks(topology) })}</Tag>
        </Space>
        {highlight && (highlight.gpus.length || highlight.links.length || highlight.numa.length) ? (
          <Tag color="processing">{t("topology.scheduleHighlight")}</Tag>
        ) : (
          <Tag>{t("topology.view")}</Tag>
        )}
      </div>

      <svg viewBox={`0 0 ${vbW} ${vbH}`} width="100%" style={{ minWidth: vbW, display: "block", fontFamily: "inherit" }}>
        <defs>
          <filter id="gpu-glow" x="-40%" y="-40%" width="180%" height="180%">
            <feDropShadow dx="0" dy="1" stdDeviation="2" floodColor={colors.chartPrimary} floodOpacity="0.22" />
          </filter>
        </defs>

        {layouts.map(({ group, x, y, w, h }) => {
          const isHl = hlNuma.has(group.numa);
          return (
            <g key={`band-${group.key}`}>
              <rect
                x={x}
                y={y}
                width={w}
                height={h}
                rx={3}
                fill={isHl ? colors.primarySoft : colors.canvas}
                stroke={isHl ? colors.chartPrimary : colors.chartGrid}
                strokeWidth={isHl ? 1.5 : 1}
                strokeDasharray={isHl ? undefined : "4 3"}
              />
              <rect x={x} y={y} width={4} height={h} rx={2} fill={isHl ? colors.chartPrimary : colors.controlBorder} />
              <text x={x + 14} y={y + 17} fontSize={12} fontWeight={700} fill={colors.ink}>
                {laneTitle(group, t)}
              </text>
              <text x={x + 86} y={y + 17} fontSize={11} fill={colors.muted}>
                {laneSubtitle(group, t)}
              </text>
            </g>
          );
        })}

        {cliques.map((clique, idx) => {
          const bounds = cliqueBounds(clique, positions);
          if (!bounds || clique.length < 2) return null;
          const anyHl = clique.some((gpu) => hlGpus.has(gpu));
          return (
            <g key={`clique-${idx}`}>
              <rect
                x={bounds.x}
                y={bounds.y}
                width={bounds.w}
                height={bounds.h}
                rx={3}
                fill={anyHl ? colors.primarySoft : colors.successSoft}
                stroke={anyHl ? colors.chartPrimary : colors.success}
                strokeWidth={1}
                strokeDasharray="4 4"
              />
              <text
                x={bounds.x + 12}
                y={bounds.y + 15}
                fontSize={10}
                fontWeight={600}
                fill={anyHl ? colors.chartPrimary : colors.successText}
              >
                {clique.length}-GPU clique
              </text>
            </g>
          );
        })}

        {edges}
        {labels}

        {idxs.map((idx) => {
          const p = positions[idx];
          const gpu = gpuMap.get(idx);
          const isHl = hlGpus.has(idx);
          // The server's verdict, as everywhere else: a card can be well
          // and still take no work.
          const healthy = gpu?.schedulable ?? true;
          const fill = isHl ? colors.primarySoft : healthy ? colors.white : colors.dangerSoft;
          const stroke = isHl ? colors.chartPrimary : healthy ? colors.weak : colors.danger;
          const title = gpu ? `${gpu.name} · ${gpuMeta(gpu, t)}` : `GPU${idx}`;
          return (
            <g key={`gpu-${idx}`} filter={isHl ? "url(#gpu-glow)" : undefined}>
              <rect
                x={p.x}
                y={p.y}
                width={CARD_W}
                height={CARD_H}
                rx={3}
                fill={fill}
                stroke={stroke}
                strokeWidth={isHl ? 2 : 1.25}
              />
              <circle cx={p.x + 14} cy={p.y + 14} r={5} fill={healthy ? colors.success : colors.danger} />
              <text x={p.x + 26} y={p.y + 18} fontSize={11} fontWeight={700} fill={colors.ink}>
                GPU{idx}
              </text>
              <text x={p.x + 14} y={p.y + 36} fontSize={10} fill={colors.text}>
                {shortText(gpu?.name || "GPU", 18)}
              </text>
              <text x={p.x + 14} y={p.y + 50} fontSize={10} fill={colors.muted}>
                {shortText(gpuMeta(gpu, t), 20)}
              </text>
              <title>{title}</title>
            </g>
          );
        })}
      </svg>

      <Legend />
    </div>
  );
}

function countFastLinks(topo: Topology): number {
  const matrix = topo.matrix ?? [];
  let count = 0;
  for (let i = 0; i < matrix.length; i++) {
    for (let j = i + 1; j < (matrix[i]?.length ?? 0); j++) {
      const link = matrix[i]?.[j];
      if (link && isFastRaw(link)) count++;
    }
  }
  return count;
}

function Legend() {
  const { t } = useTranslation("nodes");
  const item = (color: string, w: number, text: string) => (
    <span style={{ display: "inline-flex", alignItems: "center", gap: 6, marginRight: 16 }}>
      <svg width={22} height={10}>
        <line x1={0} y1={5} x2={22} y2={5} stroke={color} strokeWidth={w} strokeLinecap="round" />
      </svg>
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        {text}
      </Typography.Text>
    </span>
  );

  return (
    <div style={{ marginTop: 10, display: "flex", flexWrap: "wrap", gap: 8, alignItems: "center" }}>
      {item(colors.success, 3.25, t("topology.legend.fast"))}
      {item(colors.border, 1.5, t("topology.legend.other"))}
      {item(colors.chartPrimary, 3.25, t("topology.legend.selected"))}
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        {t("topology.legend.cardDescription")}
      </Typography.Text>
    </div>
  );
}

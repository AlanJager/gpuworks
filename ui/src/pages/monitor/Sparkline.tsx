import { colors } from "../../theme/tokens";

const WIDTH = 80;
const HEIGHT = 24;

// Sparkline is a plain inline SVG, deliberately not an ECharts instance: it
// renders inside a small table cell / tile where mounting a full chart per
// row would be wasteful (plan §7: "80×24 内联 SVG，不起 ECharts 实例").
export default function Sparkline({
  points,
  color = colors.chartPrimary,
}: {
  points?: Array<[number, number]>;
  color?: string;
}) {
  const values = (points ?? []).map(([, v]) => v).filter((v) => Number.isFinite(v));
  if (values.length < 2) {
    return <svg width={WIDTH} height={HEIGHT} viewBox={`0 0 ${WIDTH} ${HEIGHT}`} aria-hidden="true" />;
  }
  const min = Math.min(...values);
  const max = Math.max(...values);
  const span = max - min || 1;
  const stepX = WIDTH / (values.length - 1);
  const coords = values.map((v, i) => {
    const x = i * stepX;
    const y = HEIGHT - ((v - min) / span) * (HEIGHT - 2) - 1;
    return [x, y] as [number, number];
  });
  const line = coords.map(([x, y]) => `${x.toFixed(1)},${y.toFixed(1)}`).join(" ");
  const area = `0,${HEIGHT} ${line} ${WIDTH},${HEIGHT}`;

  return (
    <svg width={WIDTH} height={HEIGHT} viewBox={`0 0 ${WIDTH} ${HEIGHT}`} aria-hidden="true">
      <polygon points={area} fill={color} opacity={0.12} />
      <polyline points={line} fill="none" stroke={color} strokeWidth={1.5} strokeLinejoin="round" strokeLinecap="round" />
    </svg>
  );
}

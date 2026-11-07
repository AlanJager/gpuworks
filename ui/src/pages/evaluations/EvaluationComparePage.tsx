import { Alert, Button, Card, Col, Row, Segmented, Space, Spin, Table, Tag, Typography } from "antd";
import type { ColumnsType } from "antd/es/table";
import { ArrowLeftOutlined } from "@ant-design/icons";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { api, usePolling, type BenchmarkPoint, type BenchmarkRun } from "../../api";
import PageHeader from "../../components/PageHeader";
import { format, interactivity, isOpenLoop, pointLoadLabel, verdictPresentation } from "./evaluationUtils";
import { useState } from "react";

const colors = ["#1677ff", "#fa8c16", "#52c41a", "#722ed1", "#eb2f96"];
type ComparePoint = Pick<BenchmarkPoint, "concurrency" | "request_rate" | "output_tput" | "request_tput" | "p50_tpot" | "p99_ttft" | "p99_tpot" | "p99_latency">;
type Series = { key: string; name: string; color: string; run: BenchmarkRun; points: ComparePoint[] };
type CompareMetric = "output_tput" | "request_tput" | "interactivity" | "p99_ttft" | "p99_tpot" | "p99_latency";

const compareValue = (point: ComparePoint, metric: CompareMetric) => metric === "interactivity" ? interactivity(point.p50_tpot) ?? 0 : Number(point[metric]) || 0;
// Points of different runs line up by their load: concurrency for closed loop,
// arrival rate for open loop.
const loadKey = (point: ComparePoint) => isOpenLoop(point) ? `r:${point.request_rate}` : `c:${point.concurrency}`;

export default function EvaluationComparePage() {
	const { t } = useTranslation("evaluations");
	const navigate = useNavigate();
	const [params] = useSearchParams();
	const ids = parseIDs(params.get("ids"));
	const [metric, setMetric] = useState<CompareMetric>("output_tput");
	const runsQuery = usePolling(() => Promise.all(ids.map((id) => api.benchmark(id))), 10000, [ids.join(",")]);
	if (runsQuery.loading) return <Card><Spin /></Card>;
	if (runsQuery.error) return <Card><Alert type="error" showIcon message={t("compare.loadFailed")} description={runsQuery.error.message} action={<Button onClick={() => navigate("/evaluations")}>{t("common.back")}</Button>} /></Card>;
	const series: Series[] = (runsQuery.data || []).map((run, index) => ({ key: `run-${run.id}`, name: run.name || `Run #${run.id}`, color: colors[index], run, points: run.points || [] }));
	const selectedCount = series.length;
	const unknownGPU = series.some((item) => item.run.target_kind === "external" && !item.run.num_gpus);
	const diff = parameterDiff(series, t);

	return <div>
		<PageHeader title={t("compare.title")} description={t("compare.description")} parents={[<Link key="list" to="/evaluations">{t("list.breadcrumb.evaluations")}</Link>]} extra={<Button icon={<ArrowLeftOutlined />} onClick={() => navigate("/evaluations")}>{t("common.back")}</Button>} />
		{selectedCount < 2 || selectedCount > 5 ? <Alert type="warning" showIcon message={t("compare.selectCount")} /> : <Space direction="vertical" size={16} style={{ width: "100%" }}>
			{unknownGPU && <Alert type="warning" showIcon message={t("compare.unknownGpu")} />}
			<Row gutter={[12, 12]}>{series.map((item) => <Col xs={24} md={12} xl={Math.max(6, Math.floor(24 / series.length))} key={item.key}><Card title={<Space><Tag color={item.color}>●</Tag>{item.name}</Space>} style={{ height: "100%" }}><Typography.Title level={5}>{verdictPresentation(item.run, t).title}</Typography.Title><Typography.Text type="secondary">{verdictPresentation(item.run, t).description}</Typography.Text></Card></Col>)}</Row>
			<Card title={t("compare.difference")}><Table rowKey="key" pagination={false} dataSource={diff} columns={[{ title: t("compare.parameter"), dataIndex: "label", fixed: "left" }, ...series.map((item) => ({ title: item.name, dataIndex: item.key, render: (value: string) => <Typography.Text mark={diff.length > 0}>{value}</Typography.Text> }))] as ColumnsType<Record<string, string>>} scroll={{ x: 260 + series.length * 180 }} /></Card>
			<Row gutter={[16, 16]}><Col xs={24} xl={12}><Card title={t("compare.pareto")}><OverlayPareto series={series} t={t} /></Card></Col><Col xs={24} xl={12}><Card title={t("compare.metrics")} extra={<Segmented size="small" value={metric} onChange={(value) => setMetric(value as typeof metric)} options={[{ value: "output_tput", label: t("detail.curveMetric.output") }, { value: "request_tput", label: t("detail.curveMetric.request") }, { value: "interactivity", label: t("compare.metricInteractivity") }, { value: "p99_ttft", label: "TTFT P99" }, { value: "p99_tpot", label: "TPOT P99" }, { value: "p99_latency", label: t("compare.metricLatency") }]} />}><GroupedBars series={series} metric={metric} t={t} /></Card></Col></Row>
		</Space>}
	</div>;
}

function parseIDs(raw: string | null) {
	return [...new Set((raw || "").split(",").map(Number).filter((value) => Number.isInteger(value) && value > 0))].slice(0, 5);
}

function parameterDiff(series: Series[], t: TFunction) {
	const rows = [
		["mode", t("compare.diff.mode"), (item: Series) => item.run.mode],
		["model", t("compare.diff.model"), (item: Series) => item.run.model || "—"],
		["isl", "ISL", (item: Series) => item.run.search?.isl?.join(",") || "—"],
		["osl", "OSL", (item: Series) => item.run.search?.osl?.join(",") || "—"],
		["dataset", t("compare.diff.dataset"), (item: Series) => item.run.dataset?.kind || "—"],
		["maxConcurrency", t("compare.diff.maxConcurrency"), (item: Series) => String(item.run.search?.max_concurrency || "—")],
		["requestRates", t("compare.diff.requestRates"), (item: Series) => item.run.search?.request_rates?.join(",") || "—"],
		["warmup", t("compare.diff.warmup"), (item: Series) => String(item.run.search?.warmup_requests ?? 0)],
		["requestTimeout", t("compare.diff.requestTimeout"), (item: Series) => item.run.search?.request_timeout_s ? `${item.run.search.request_timeout_s}s` : "—"],
		["ignoreEOS", t("compare.diff.ignoreEOS"), (item: Series) => item.run.dataset?.ignore_eos == null ? "—" : t(item.run.dataset.ignore_eos ? "compare.diff.on" : "compare.diff.off")],
		["gain", t("compare.diff.gain"), (item: Series) => item.run.search?.min_throughput_gain == null ? "—" : `${Number(item.run.search.min_throughput_gain) * 100}%`],
		["sla", t("compare.diff.sla"), (item: Series) => item.run.mode === "sla" ? JSON.stringify(Object.fromEntries(Object.entries(item.run.search || {}).filter(([key]) => key.startsWith("min_") || key.startsWith("max_") && key !== "max_concurrency" && key !== "max_in_flight"))) : "—"],
	] as const;
	return rows.flatMap(([key, label, getter]) => {
		const values = series.map(getter);
		if (new Set(values).size <= 1) return [];
		const row: Record<string, string> = { key, label };
		series.forEach((item, index) => { row[item.key] = values[index]; });
		return [row];
	});
}

function OverlayPareto({ series, t }: { series: Series[]; t: TFunction }) {
	const width = 680, height = 300, pad = 42;
	// Interactivity (1000 / TPOT P50) against output throughput; points without
	// a measured TPOT have no interactivity and are not drawn.
	const plotted = series.map((item) => ({ item, points: item.points.flatMap((point) => { const x = interactivity(point.p50_tpot); return x == null ? [] : [{ point, x, y: point.output_tput }]; }).sort((a, b) => a.x - b.x) }));
	const all = plotted.flatMap((entry) => entry.points);
	const maxX = Math.max(1, ...all.map((item) => item.x)), maxY = Math.max(1, ...all.map((item) => item.y));
	const xy = (x: number, y: number) => [pad + x / maxX * (width - pad * 2), height - pad - y / maxY * (height - pad * 1.5)];
	return <div style={{ overflowX: "auto" }}><svg role="img" aria-label={t("compare.multiParetoAria")} viewBox={`0 0 ${width} ${height}`} style={{ minWidth: 520, width: "100%" }}><line x1={pad} y1={height - pad} x2={width - pad} y2={height - pad} stroke="#98a2b3" /><line x1={pad} y1={20} x2={pad} y2={height - pad} stroke="#98a2b3" />{plotted.map(({ item, points }) => { const coords = points.map((entry) => xy(entry.x, entry.y)); return <g key={item.key}><polyline fill="none" stroke={item.color} strokeWidth="2" points={coords.map((point) => point.join(",")).join(" ")} />{coords.map(([x, y], index) => <circle key={index} cx={x} cy={y} r="5" fill={item.color}><title>{`${item.name} · ${pointLoadLabel(points[index].point)} · ${format(points[index].x)} tok/s/user · ${format(points[index].y)} tok/s`}</title></circle>)}</g>; })}<text x={width / 2} y={height - 8} textAnchor="middle" fontSize="12">{t("pareto.xAxis")}</text></svg><Space wrap>{series.map((item) => <Tag key={item.key} color={item.color}>{item.name}</Tag>)}</Space></div>;
}

function GroupedBars({ series, metric, t }: { series: Series[]; metric: CompareMetric; t: TFunction }) {
	const loads = new Map<string, ComparePoint>();
	for (const point of series.flatMap((item) => item.points)) if (!loads.has(loadKey(point))) loads.set(loadKey(point), point);
	const ordered = [...loads.entries()].sort(([, a], [, b]) => Number(isOpenLoop(a)) - Number(isOpenLoop(b)) || (a.request_rate ?? 0) - (b.request_rate ?? 0) || a.concurrency - b.concurrency);
	const max = Math.max(1, ...series.flatMap((item) => item.points.map((point) => compareValue(point, metric))));
	return <Space direction="vertical" style={{ width: "100%" }}>{ordered.map(([key, sample]) => <div key={key}><Typography.Text strong>{isOpenLoop(sample) ? t("compare.rate", { value: format(sample.request_rate, 2) }) : t("compare.concurrency", { value: sample.concurrency })}</Typography.Text>{series.map((item) => { const point = item.points.find((candidate) => loadKey(candidate) === key); const value = point ? compareValue(point, metric) : 0; return <div key={item.key} style={{ display: "grid", gridTemplateColumns: "130px 1fr 72px", gap: 8, alignItems: "center", marginTop: 6 }}><Typography.Text ellipsis>{item.name}</Typography.Text><div style={{ height: 14, background: "#f2f4f7", borderRadius: 3 }}><div style={{ width: `${value / max * 100}%`, height: "100%", background: item.color, borderRadius: 3 }} /></div><Typography.Text>{point ? format(value) : "—"}</Typography.Text></div>; })}</div>)}</Space>;
}

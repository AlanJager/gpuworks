import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ArrowLeftOutlined, CopyOutlined, StopOutlined } from "@ant-design/icons";
import { Alert, Button, Card, Col, Popconfirm, Progress, Row, Segmented, Space, Spin, Typography, message } from "antd";
import { useNavigate, useParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api, useEventPayload, usePolling, type BenchmarkCurveResponse, type BenchmarkCurveSample, type BenchmarkPoint, type BenchmarkRun, type MetricPoint, type MetricSeries } from "../../api";
import PageHeader from "../../components/PageHeader";
import TimeSeriesChart from "../../components/TimeSeriesChart";
import { AdvancedResult, Metric } from "../EvaluationSupport";
import BenchmarkWizard, { type WizardPreset } from "./BenchmarkWizard";
import ParetoChart from "./ParetoChart";
import PointResultsTable from "./PointResultsTable";
import VerdictCard from "./VerdictCard";
import { benchmarkSeriesName, bestPoint, metricCardState, pointParameterLabel } from "./evaluationUtils";
import { createRefreshCoalescer } from "./evaluationRefresh";
import { localizedText } from "../../i18n/localizedText";
import { BenchmarkSnapshotPanel, BenchmarkTargetCard, benchmarkDatasetProfile, benchmarkReusesSamples } from "./BenchmarkSnapshotPanel";
import BenchmarkCurveNotice from "./BenchmarkCurveNotice";
import BenchmarkReportMenu from "./BenchmarkReportMenu";
import MetricLabel from "./MetricLabel";
import BenchmarkEvidencePanel from "./BenchmarkEvidencePanel";
import { useWizardTargets } from "./useWizardTargets";

const curveFields: Record<string, keyof Omit<BenchmarkCurveSample, "t">> = {
	gpuworks_bench_output_tput: "output_tput",
	gpuworks_bench_total_tput: "total_tput",
	gpuworks_bench_request_tput: "request_tput",
	gpuworks_bench_ttft_p50_ms: "ttft_p50",
	gpuworks_bench_tpot_p50_ms: "tpot_p50",
	gpuworks_bench_latency_p99_ms: "latency_p99",
};

export default function EvaluationDetailPage() {
	const { t, i18n } = useTranslation("evaluations");
	const language = i18n.resolvedLanguage || i18n.language;
	const curveMetrics = useMemo(() => [
		{ value: "gpuworks_bench_output_tput", label: t("detail.curveMetric.output"), unit: "tok/s" },
		{ value: "gpuworks_bench_total_tput", label: t("detail.curveMetric.total"), unit: "tok/s" },
		{ value: "gpuworks_bench_request_tput", label: t("detail.curveMetric.request"), unit: "req/s" },
		{ value: "gpuworks_bench_ttft_p50_ms", label: "TTFT", unit: "ms" },
		{ value: "gpuworks_bench_tpot_p50_ms", label: "TPOT", unit: "ms" },
		{ value: "gpuworks_bench_latency_p99_ms", label: t("detail.curveMetric.latency"), unit: "ms" },
	], [t]);
	const id = Number(useParams().id);
	const navigate = useNavigate();
	const [chartMetric, setChartMetric] = useState(curveMetrics[0].value);
	const [selectedSeq, setSelectedSeq] = useState<number>();
	const [wizardPreset, setWizardPreset] = useState<WizardPreset>();
	const runQuery = usePolling<BenchmarkRun>(() => api.benchmark(id), 0, [id]);
	const metaQuery = usePolling(api.benchmarkMeta, 0);
	const run = runQuery.data;
	const active = run?.state === "pending" || run?.state === "running";
	const curve = usePolling<BenchmarkCurveResponse>(() => api.benchmarkCurve(id), 0, [id]);
	const wizardOpen = !!wizardPreset;
	const targets = useWizardTargets(wizardOpen);
	const refreshRef = useRef({ run: runQuery.refresh, curve: curve.refresh });
	refreshRef.current = { run: runQuery.refresh, curve: curve.refresh };
	const coalescer = useMemo(() => createRefreshCoalescer(async () => { await Promise.all([refreshRef.current.run(), refreshRef.current.curve()]); }, 1000), [id]);
	useEffect(() => () => coalescer.dispose(), [coalescer]);
	useEventPayload("benchmark", (event) => { if (event.key === String(id)) coalescer.trigger(); });
	useEffect(() => {
		if (!active) return;
		const fallback = setInterval(() => coalescer.trigger(), 10000);
		return () => clearInterval(fallback);
	}, [active, coalescer]);

	const best = (run?.points?.length ? bestPoint(run) : undefined) as BenchmarkPoint | undefined;
	const selected = run?.points?.find((point) => point.seq === selectedSeq) || best;
	const liveCards = run ? metricCardState(run, t) : undefined;
	const curveSeriesName = useCallback((labels: Record<string, string>, index: number) => run ? benchmarkSeriesName(run, labels, index, t) : t("common.point", { value: index + 1 }), [run, t]);
	const chart = useMemo<MetricSeries[]>(() => {
		const field = curveFields[chartMetric];
		return (curve.data?.points || []).map((point) => ({ labels: { benchmark: String(id), point: String(point.seq) }, points: point.samples.map((sample): MetricPoint => [sample.t, Number(sample[field])]) }));
	}, [chartMetric, curve.data?.points, id]);
	const curveHasSamples = chart.some((series) => series.points.length > 0);

	if (runQuery.loading && !runQuery.data || metaQuery.loading && !metaQuery.data) return <Card><Spin /></Card>;
	if (runQuery.error || metaQuery.error || !run || !metaQuery.data) return <Card><Alert type="error" showIcon message={t("detail.loadFailed")} description={(runQuery.error || metaQuery.error)?.message || t("detail.missing")} action={<Button onClick={() => navigate("/evaluations")}>{t("common.back")}</Button>} /></Card>;
	const meta = metaQuery.data;
	const progressPct = run.mode === "grid" ? Math.round((Math.max(0, (run.progress?.point_index || 1) - 1) / Math.max(1, run.progress?.point_total || 1)) * 100) : undefined;
	const completed = run.progress?.completed ?? 0;
	const requestTotal = run.progress?.current_point?.request_total;
	const datasetProfile = benchmarkDatasetProfile(run, meta.dataset_profiles || {});
	const reusesSamples = benchmarkReusesSamples(run, datasetProfile);

	return <div>
		<PageHeader title={run.name || `Run #${run.id}`} description={`${localizedText(meta.modes.find((item) => item.value === run.mode)?.label, language) || run.mode} · ${run.model}`} breadcrumb={[t("list.breadcrumb.services"), t("list.breadcrumb.evaluations"), run.name || `#${run.id}`]} extra={<Space><Button icon={<ArrowLeftOutlined />} onClick={() => navigate("/evaluations")}>{t("common.back")}</Button><BenchmarkReportMenu runID={run.id} /><Button icon={<CopyOutlined />} onClick={() => setWizardPreset({ sourceRun: run })}>{t("detail.rerun")}</Button>{active && <Popconfirm title={t("detail.cancelTitle")} description={t("detail.cancelDescription")} onConfirm={async () => { try { await api.cancelBenchmark(run.id); await runQuery.refetch(); message.success(t("detail.canceled")); } catch (error) { message.error((error as Error).message); } }}><Button danger icon={<StopOutlined />}>{t("detail.cancel")}</Button></Popconfirm>}</Space>} />
			<Space direction="vertical" size={16} style={{ width: "100%" }}>
				<VerdictCard run={run} meta={meta} onRerun={(maxConcurrency) => setWizardPreset({ sourceRun: run, maxConcurrency })} />
				{run.warnings?.map((warning) => <Alert key={warning} type="warning" showIcon message={t(`benchmarkWarning.${warning}`, { defaultValue: warning })} />)}
				{reusesSamples && <Alert type="warning" showIcon message={t("detail.sampleReuseWarning")} />}
				<BenchmarkTargetCard run={run} />
			{run.mode === "grid" && active && <Card size="small"><Progress percent={progressPct} status="active" /><Typography.Text type="secondary">{t("detail.gridProgress", { completed, total: requestTotal ? ` / ${requestTotal}` : "" })}</Typography.Text></Card>}
			<Card size="small" title={liveCards?.label}>
				{liveCards?.point && <Typography.Text type="secondary" style={{ display: "block", marginBottom: 12 }}>{pointParameterLabel(liveCards.point)}{active && requestTotal ? ` · ${t("detail.requestProgress", { completed, total: requestTotal })}` : ""}</Typography.Text>}
				<Row gutter={[10, 10]}>
					<Metric title={<MetricLabel label={t("detail.metric.output")} help="output" />} value={liveCards?.values?.outputTput} suffix="tok/s" /><Metric title={<MetricLabel label={t("detail.metric.request")} help="request" />} value={liveCards?.values?.requestTput} suffix="req/s" /><Metric title={<MetricLabel label={t("detail.metric.interactivity")} help="interactivity" />} value={liveCards?.values?.interactivity} suffix="tok/s/user" /><Metric title={<MetricLabel label={t("detail.metric.total")} help="total" />} value={liveCards?.values?.totalTput} suffix="tok/s" /><Metric title={<MetricLabel label="TTFT P50" help="ttft" />} value={liveCards?.values?.ttftP50} suffix="ms" /><Metric title={<MetricLabel label="TPOT P50" help="tpot" />} value={liveCards?.values?.tpotP50} suffix="ms" /><Metric title={<MetricLabel label={t("detail.metric.success")} help="success" />} value={liveCards?.values?.successRate} suffix="%" /><Metric title={<MetricLabel label={t("detail.metric.latency")} help="latency" />} value={liveCards?.values?.latencyP99} suffix="ms" />
				</Row>
			</Card>
			<Row gutter={[16, 16]}>
					<Col xs={24} xl={12}><Card title={t("detail.curve")} extra={<Segmented size="small" value={chartMetric} onChange={(value) => setChartMetric(String(value))} options={curveMetrics.map(({ value, label }) => ({ value, label }))} />}><BenchmarkCurveNotice active={active} loading={curve.loading} hasResponse={curve.data != null} hasSamples={curveHasSamples} runState={run.state} completed={run.progress?.completed ?? 0} runError={run.error} error={curve.error} onRetry={() => { void curve.refetch(); }} />{!(curve.loading && curve.data == null) && <TimeSeriesChart series={chart} height={280} unit={curveMetrics.find((item) => item.value === chartMetric)?.unit} seriesName={curveSeriesName} resetKey={`${id}:${chartMetric}`} />}</Card></Col>
				<Col xs={24} xl={12}><Card title={<MetricLabel label={t("detail.pareto")} help="pareto" />}><ParetoChart points={run.points || []} selectedSeq={selected?.seq} onSelect={(point) => setSelectedSeq(point.seq)} /></Card></Col>
			</Row>
			<PointResultsTable run={run} selectedSeq={selected?.seq} onSelect={(point) => setSelectedSeq(point.seq)} />
			<BenchmarkEvidencePanel run={run} point={selected} />
			<AdvancedResult run={run} point={selected} />
				<BenchmarkSnapshotPanel run={run} />
		</Space>
		<BenchmarkWizard open={wizardOpen} meta={meta} deployments={targets.deployments} nodes={targets.nodes} onSearchDeployments={targets.searchDeployments} onSearchNodes={targets.searchNodes} preset={wizardPreset} onClose={() => setWizardPreset(undefined)} onCreated={(created) => { setWizardPreset(undefined); navigate(`/evaluations/${created.id}`); }} />
	</div>;
}

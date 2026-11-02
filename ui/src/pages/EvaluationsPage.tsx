import { useEffect, useMemo, useRef, useState } from "react";
import { CopyOutlined, DeleteOutlined, DiffOutlined, PlusOutlined } from "@ant-design/icons";
import { Alert, Button, Card, Col, Popconfirm, Row, Space, Spin, Tag, Tooltip, Typography, message } from "antd";
import ResourceTable, { type ResourceColumn } from "../components/resourceTable/ResourceTable";
import { useNavigate, useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { api, useEvents, usePolling, type BenchmarkRun, type List } from "../api";
import EmptyState from "../components/EmptyState";
import PageHeader from "../components/PageHeader";
import BenchmarkWizard, { type WizardPreset } from "./evaluations/BenchmarkWizard";
import { bestPoint, deletedTargetLabelKeys, format, targetLabel } from "./evaluations/evaluationUtils";
import { createRefreshCoalescer } from "./evaluations/evaluationRefresh";
import { formatTime } from "../i18n/datetime";
import { localizedText } from "../i18n/localizedText";
import { useWizardTargets } from "./evaluations/useWizardTargets";

export default function EvaluationsPage() {
	const { t, i18n } = useTranslation("evaluations");
	const language = i18n.resolvedLanguage || i18n.language;
	const navigate = useNavigate();
	const [params, setParams] = useSearchParams();
	const runsQuery = usePolling<List<BenchmarkRun>>(api.benchmarks, 3000);
	const metaQuery = usePolling(api.benchmarkMeta, 60000);
	const refreshRef = useRef(runsQuery.refresh);
	refreshRef.current = runsQuery.refresh;
	const refreshCoalescer = useMemo(() => createRefreshCoalescer(() => refreshRef.current(), 1000), []);
	useEffect(() => () => refreshCoalescer.dispose(), [refreshCoalescer]);
	useEvents("benchmark", refreshCoalescer.trigger);
	useEvents("deployment", refreshCoalescer.trigger);
	const [selected, setSelected] = useState<number[]>([]);
	const [localPreset, setLocalPreset] = useState<WizardPreset>();
	const open = params.has("create") || !!localPreset;
	const runs = runsQuery.data?.items ?? [];
	const queryPreset: WizardPreset | undefined = params.has("create") ? {
		mode: (["grid", "saturation", "sla"].includes(params.get("create") || "") ? params.get("create") : undefined) as BenchmarkRun["mode"] | undefined,
		deploymentID: Number(params.get("deployment_id")) || undefined,
	} : undefined;
	const preset = localPreset || queryPreset;
	const targets = useWizardTargets(open);
	const meta = metaQuery.data;
	const initialLoading = (runsQuery.loading && !runsQuery.data) || (metaQuery.loading && !metaQuery.data);

	const closeWizard = () => {
		setLocalPreset(undefined);
		if (params.has("create")) {
			const next = new URLSearchParams(params);
			next.delete("create"); next.delete("deployment_id");
			setParams(next, { replace: true });
		}
	};
	const compare = () => {
		if (selected.length < 2 || selected.length > 5) return;
		const search = new URLSearchParams();
		search.set("ids", selected.join(","));
		navigate(`/evaluations/compare?${search}`);
	};

	const modeLabel = (mode: string) => localizedText(meta?.modes.find((item) => item.value === mode)?.label, language) || mode;
	const statusLabel = (state: string) => localizedText(meta?.statuses.find((status) => status.value === state)?.label, language) || state;
	const columns: ResourceColumn<BenchmarkRun>[] = [
		{ key: "name", role: "name", title: t("list.column.name"), width: 280, sortValue: (run) => run.name || `Run #${run.id}`, render: (_, run) => <Button type="link" onClick={() => navigate(`/evaluations/${run.id}`)}>{run.name || `Run #${run.id}`}</Button> },
		{ key: "target", title: t("list.column.target"), width: 220, sortValue: (run) => targetLabel(run, run.target_snapshot?.deployment_name, t), render: (_, run) => <Space size={6}>{targetLabel(run, run.target_snapshot?.deployment_name, t)}{deletedTargetLabelKeys(run).map((key) => <Tag key={key}>{t(key)}</Tag>)}</Space> },
		{ key: "mode", title: t("list.column.mode"), width: 150, sortValue: (run) => modeLabel(run.mode), filters: (meta?.modes ?? []).map((mode) => ({ value: mode.value, text: localizedText(mode.label, language) })), onFilter: (value, run) => run.mode === value, render: (_, run) => modeLabel(run.mode) },
		{ key: "state", title: t("list.column.status"), width: 120, sortValue: (run) => statusLabel(run.state), filters: (meta?.statuses ?? []).map((status) => ({ value: status.value, text: localizedText(status.label, language) })), onFilter: (value, run) => run.state === value, render: (_, run) => { const item = meta?.statuses.find((status) => status.value === run.state); return <Tag color={item?.color || "default"}>{statusLabel(run.state)}</Tag>; } },
		{ key: "verdict", title: t("list.column.verdict"), width: 260, render: (_, run) => verdictSummary(run, t) },
		{ key: "created_at", title: t("list.column.time"), width: 180, sortValue: (run) => run.created_at, render: (_, run) => formatTime(run.created_at) },
		{ key: "actions", role: "actions", title: t("list.column.actions"), width: 120, render: (_, run) => <Space size={2}>
			<Tooltip title={t("list.rerun")}><Button type="text" size="small" icon={<CopyOutlined />} aria-label={t("list.rerun")} onClick={() => setLocalPreset({ sourceRun: run.deployment_deleted || run.instance_deleted ? { ...run, deployment_id: undefined, instance_uuid: undefined } : run })} /></Tooltip>
			<Popconfirm title={t("list.deleteTitle")} description={run.state === "running" || run.state === "pending" ? t("list.deleteRunning") : t("list.deleteDescription")} disabled={run.state === "running" || run.state === "pending"} onConfirm={async () => { try { await api.deleteBenchmark(run.id); setSelected((ids) => ids.filter((id) => id !== run.id)); await runsQuery.refetch(); message.success(t("list.deleted")); } catch (error) { message.error((error as Error).message); } }}>
				<Tooltip title={t("list.delete")}><Button danger type="text" size="small" disabled={run.state === "running" || run.state === "pending"} icon={<DeleteOutlined />} aria-label={t("list.delete")} /></Tooltip>
			</Popconfirm>
		</Space> },
	];

	return <div>
		<PageHeader title={t("list.title")} description={t("list.description")} extra={<Space><Button icon={<DiffOutlined />} disabled={selected.length < 2 || selected.length > 5} onClick={compare}>{t("list.compare", { count: selected.length })}</Button><Button type="primary" icon={<PlusOutlined />} onClick={() => setLocalPreset({})}>{t("list.new")}</Button></Space>} />
		{(runsQuery.error || metaQuery.error) && <Alert type="error" showIcon message={t("list.loadFailed")} description={(runsQuery.error || metaQuery.error)?.message} style={{ marginBottom: 16 }} action={<Button size="small" onClick={() => { void runsQuery.refetch(); void metaQuery.refetch(); }}>{t("common.retry")}</Button>} />}
		{initialLoading ? <Card><Spin /></Card> : !runs.length ? <Card><EmptyState description={t("list.empty")} action={<Row gutter={[12, 12]}>{meta?.modes.map((mode) => <Col xs={24} md={8} key={mode.value}><Card hoverable style={{ height: "100%" }} title={localizedText(mode.label, language)} onClick={() => setLocalPreset({ mode: mode.value as BenchmarkRun["mode"] })}><Typography.Text type="secondary">{localizedText(mode.description, language)}</Typography.Text></Card></Col>)}</Row>} /></Card> : <Card>
			<ResourceTable<BenchmarkRun> tableId="evaluations" rowKey="id" data={runs} columns={columns} onRefresh={() => void runsQuery.refetch()} rowSelection={{ selectedRowKeys: selected, onChange: (keys) => setSelected(keys.map(Number)), getCheckboxProps: (run) => ({ disabled: !selected.includes(run.id) && selected.length >= 5 }) }} />
		</Card>}
		{meta && <BenchmarkWizard open={open} meta={meta} deployments={targets.deployments} nodes={targets.nodes} onSearchDeployments={targets.searchDeployments} onSearchNodes={targets.searchNodes} preset={preset} onClose={closeWizard} onCreated={(run) => { closeWizard(); void runsQuery.refetch(); navigate(`/evaluations/${run.id}`); }} />}
	</div>;
}

function verdictSummary(run: BenchmarkRun, t: TFunction) {
	const verdict = run.verdict;
	const point = bestPoint(run);
	if (!verdict) return "—";
	if (verdict.kind === "saturated") return point ? `${t("list.verdict.saturated", { concurrency: verdict.saturation_concurrency })} · ${format(point.output_tput)} tok/s` : t("list.verdict.saturated", { concurrency: verdict.saturation_concurrency });
	if (verdict.kind === "truncated") return <Tag color="orange">{t("list.verdict.truncated", { headroom: format(verdict.headroom_pct) })}</Tag>;
	if (verdict.kind === "sla_met") return t("list.verdict.slaMet", { concurrency: verdict.sla_max_concurrency });
	if (verdict.kind === "sla_none") return <Tag color="orange">{t("list.verdict.slaNone")}</Tag>;
	if (verdict.kind === "grid_done") return point ? `${t("list.verdict.gridDone")} · ${t("list.verdict.highest", { value: format(point.output_tput) })}` : `${t("list.verdict.gridDone")} · ${t("list.verdict.points", { count: run.progress?.point_count ?? run.progress?.point_total ?? 0 })}`;
	return point ? t("list.verdict.partial") : t("list.verdict.failed");
}

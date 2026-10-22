import { DownloadOutlined } from "@ant-design/icons";
import { Button, Card, Col, Collapse, Descriptions, Statistic, Table } from "antd";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import type { BenchmarkPoint, BenchmarkRun } from "../api";
import { pointTimingValue } from "./evaluations/evaluationUtils";

const fmt = (value?: number, digits = 1) => value == null || !Number.isFinite(value) ? "-" : value.toLocaleString(undefined, { maximumFractionDigits: digits });

export function Metric({ title, value, suffix }: { title: ReactNode; value?: number; suffix: string }) {
	return <Col xs={12} md={6}><Card size="small"><Statistic title={title} value={value == null ? "-" : Number(value.toFixed(1))} suffix={suffix} /></Card></Col>;
}

export function fmtMs(value?: number) {
	return value == null ? "-" : `${fmt(value)} ms`;
}


export function AdvancedResult({ run, point }: { run: BenchmarkRun; point?: BenchmarkPoint }) {
	const { t } = useTranslation("evaluations");
	const long = point?.long_tail;
	const evidence = long?.evidence?.version === 1 ? long.evidence : undefined;
	const speculative = evidence ? undefined : long?.speculative;
	const cache = evidence?.all.cache;
	const cacheRate = evidence ? cache && cache.input_tokens > 0 ? cache.cached_tokens / cache.input_tokens : undefined : long?.kv_hit_rate;
	const tokenSources = (sources: Record<string, number> | null | undefined) => Object.entries(sources || {})
		.filter(([, count]) => count > 0)
		.map(([source, count]) => `${t(`evidence.source.${source}`, { defaultValue: t("evidence.unknown") })}: ${count}`)
		.join(" · ") || t("evidence.unknown");
	const timingPercentile = (metric: "ttft" | "tpot" | "latency", percentile: string) => {
		if (long?.evidence?.version !== 1) return long?.[`${metric}_ms`]?.[percentile];
		if (!point || (percentile !== "50" && percentile !== "99")) return undefined;
		return pointTimingValue(point, `${percentile === "50" ? "p50" : "p99"}_${metric}`);
	};
	const rows = ["1", "5", "10", "25", "50", "75", "90", "95", "99"].map((percentile) => ({ p: `P${percentile}`, ttft: timingPercentile("ttft", percentile), tpot: timingPercentile("tpot", percentile), latency: timingPercentile("latency", percentile), itl: evidence ? undefined : long?.itl_ms?.[percentile] }));
	return <Collapse items={[{
		key: "advanced",
		label: t("support.advanced"),
		children: <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
			<Descriptions size="small" column={{ xs: 1, sm: 2 }} items={[
				{ key: "kv", label: t(evidence ? "evidence.cacheWeighted" : "support.kv"), children: cacheRate == null ? t(evidence ? "evidence.unknown" : "support.kvMissing") : `${fmt(cacheRate * 100)}%` },
				{ key: "spec", label: t("support.speculative"), children: speculative ? <><span>{t("support.speculativeValue", { tokens: fmt(speculative.tokens_per_iteration, 2), rate: fmt(speculative.acceptance_rate * 100) })}</span><br />{t("evidence.legacySpeculative")}</> : t(evidence ? "evidence.unknown" : "support.speculativeMissing") },
				{ key: "length", label: t("support.lengthMethod"), children: evidence ? <><span>{t("evidence.input")}: {tokenSources(evidence.all.tokens?.input_sources)}</span><br /><span>{t("evidence.output")}: {tokenSources(evidence.all.tokens?.output_sources)}</span></> : long?.approximate_tokens ? t("support.estimated") : t("support.exact") },
				{ key: "detail", label: t("support.detail"), children: point?.detail_path ? <Button size="small" icon={<DownloadOutlined />} href={`/api/v1/benchmarks/${run.id}/points/${point.seq}/detail`}>{t("support.download")}</Button> : t("support.detailMissing") },
			]} />
			<Table size="small" rowKey="p" pagination={false} dataSource={rows} columns={[
				{ title: t("support.percentile"), dataIndex: "p" },
				{ title: "TTFT", dataIndex: "ttft", render: (value: number) => value == null ? "-" : `${fmt(value)} ms` },
				{ title: "TPOT", dataIndex: "tpot", render: (value: number) => value == null ? "-" : `${fmt(value)} ms` },
				{ title: "Latency", dataIndex: "latency", render: (value: number) => value == null ? "-" : `${fmt(value)} ms` },
				{ title: t("evidence.chunkInterval"), dataIndex: "itl", render: (value: number) => value == null ? "-" : `${fmt(value)} ms` },
			]} />
		</div>,
	}]} />;
}

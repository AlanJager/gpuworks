import { Col, Form, InputNumber, Row, Select, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { BenchmarkMeta } from "../../api";
import { formatAgenticTokens, summarizeAgenticLoad } from "./agenticWorkload";

export default function AgenticLoadSummary({ summary, meta, onPresetChange }: { summary: ReturnType<typeof summarizeAgenticLoad>; meta: BenchmarkMeta; onPresetChange: (value: string) => void }) {
	const { t } = useTranslation("evaluations");
	return <>
		<Form.Item name="agentic_preset" label={t("agentic.meanInput")} tooltip={t("agentic.meanInputHelp")}>
			<Select onChange={onPresetChange} options={meta.agentic_presets.map((preset) => ({
				value: preset.value,
				label: preset.recommended ? `${preset.label} (${t("agentic.recommended")})` : preset.label,
			}))} />
		</Form.Item>
		<Form.Item name="agentic_mean_input" hidden><InputNumber /></Form.Item>
		<section className="agentic-load-summary agentic-wizard-summary" aria-label={t("agentic.summaryAria")}>
			<Row gutter={[8, 8]}>
				<Stat label={t("agentic.metric.peakPrompt")} value={formatAgenticTokens(summary.peakPromptTokens)} />
				<Stat label={t("agentic.metric.context")} value={formatAgenticTokens(summary.requiredContextTokens)} />
				<Stat label={t("agentic.metric.turns")} value={String(summary.turns)} />
				<Stat label={t("agentic.result.designReuse")} value={`${(summary.designPrefixReuseRate * 100).toFixed(1)}%`} />
			</Row>
			<div className="agentic-summary-foot">
				<span>{t("agentic.turnShape", { input: meta.agentic_defaults.subsequent_turn_input_tokens, output: meta.agentic_defaults.output_tokens })}</span>
				<span>{t("agentic.volume", { sessions: summary.sessions, requests: summary.requests })}</span>
			</div>
		</section>
	</>;
}

function Stat({ label, value }: { label: string; value: string }) {
	return <Col span={6}><div className="agentic-summary-stat agentic-wizard-stat"><Typography.Text type="secondary">{label}</Typography.Text><strong>{value}</strong></div></Col>;
}

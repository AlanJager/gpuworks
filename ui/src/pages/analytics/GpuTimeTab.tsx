import { useMemo } from "react";
import { Alert, Card, Space, Table, Typography } from "antd";
import type { ColumnsType } from "antd/es/table";
import { useTranslation } from "react-i18next";
import { analyticsApi, type GPUTimeAnalytics, type GPUTimeRow } from "../../analyticsApi";
import { SERIES_COLORS, stackedBarOption } from "./chartOptions";
import type { CsvModel } from "./csv";
import EChart from "./EChart";
import { useExportModel } from "./exportContext";
import { DASH, fmtCompact, fmtFixed, fmtInt, fmtPct, ratio } from "./format";
import { DeploymentCell } from "./names";
import QueryState from "./QueryState";
import StatCards, { type StatItem } from "./StatCards";
import type { TabProps } from "./types";
import { useAnalyticsData } from "./useAnalyticsData";
import { bucketLabel, spansDays } from "./zoned";

/** perMillion is GPU hours per million tokens; null without tokens. */
export function perMillion(hours: number, tokens: number): number | null {
  return tokens > 0 ? hours / (tokens / 1e6) : null;
}

/** deploymentTokens adds up each deployment's tokens once: its rows (one per card model) all carry the whole deployment's. */
export function deploymentTokens(rows: GPUTimeRow[]): { prompt: number; completion: number } {
  const seen = new Map<number, GPUTimeRow>();
  for (const r of rows) if (!seen.has(r.deployment_id)) seen.set(r.deployment_id, r);
  let prompt = 0;
  let completion = 0;
  for (const r of seen.values()) {
    prompt += r.prompt_tokens;
    completion += r.completion_tokens;
  }
  return { prompt, completion };
}

export default function GpuTimeTab({ from, to, granularity, deployment, tz, reload }: TabProps) {
  const { t, i18n } = useTranslation("analytics");
  const language = i18n.resolvedLanguage ?? "en";
  const query = useAnalyticsData(
    (signal) => analyticsApi.gpuTime({ from, to, granularity, deployment }, signal),
    [from, to, granularity, deployment, reload],
  );
  const data = query.data;

  const exportModel = useMemo<CsvModel | null>(() => {
    if (!data) return null;
    const tokens = deploymentTokens(data.rows);
    const hours = data.rows.reduce((a, r) => a + r.gpu_hours, 0);
    return {
      name: "gpu-time",
      head: [
        t("gpuTime.export.deployment"), t("gpuTime.export.servedName"), t("gpuTime.table.model"), t("gpuTime.table.avgInstances"), t("gpuTime.table.maxInstances"), t("gpuTime.table.gpuHours"),
        t("gpuTime.table.wholeHours"), t("gpuTime.table.prompt"), t("gpuTime.table.output"), t("gpuTime.table.perMillionOutput"), t("gpuTime.table.perMillionAll"),
      ],
      rows: [
        ...data.rows.map((r) => [
          r.deployment_name || t("names.deploymentId", { id: r.deployment_id }), r.served_name, r.gpu_model, r.avg_instances.toFixed(2), r.max_instances, r.gpu_hours.toFixed(2), r.whole_gpu_hours.toFixed(2),
          r.prompt_tokens, r.completion_tokens, r.gpu_hours_per_million_output == null ? "" : r.gpu_hours_per_million_output.toFixed(2), r.gpu_hours_per_million_tokens == null ? "" : r.gpu_hours_per_million_tokens.toFixed(2),
        ]),
        [t("table.total"), "", "", "", "", hours.toFixed(2), data.rows.reduce((a, r) => a + r.whole_gpu_hours, 0).toFixed(2), tokens.prompt, tokens.completion,
          perMillion(hours, tokens.completion)?.toFixed(2) ?? "", perMillion(hours, tokens.prompt + tokens.completion)?.toFixed(2) ?? ""],
      ],
    };
  }, [data, t]);
  useExportModel(exportModel);

  const cards = (d: GPUTimeAnalytics): StatItem[] => {
    const hours = d.rows.reduce((a, r) => a + r.gpu_hours, 0);
    const allocated = d.capacity.reduce((a, c) => a + c.allocated_gpu_hours, 0);
    const schedulable = d.capacity.reduce((a, c) => a + c.schedulable_gpu_hours, 0);
    const tokens = deploymentTokens(d.rows);
    const out = perMillion(hours, tokens.completion);
    const all = perMillion(hours, tokens.prompt + tokens.completion);
    return [
      { key: "hours", title: t("gpuTime.cards.hours"), value: fmtFixed(hours, 1), sub: t("gpuTime.cards.hoursSub"), tip: t("gpuTime.cards.hoursTip") },
      {
        key: "allocation", title: t("gpuTime.cards.allocation"), value: fmtPct(ratio(allocated, schedulable)), tip: t("gpuTime.cards.allocationTip"),
        sub: schedulable > 0 ? t("gpuTime.cards.allocationSub", { allocated: allocated.toFixed(1), schedulable: schedulable.toFixed(1) }) : t("gpuTime.cards.allocationNone"),
      },
      { key: "output", title: t("gpuTime.cards.output"), value: fmtCompact(tokens.completion, language), exact: fmtInt(tokens.completion), sub: t("gpuTime.cards.outputSub") },
      {
        key: "perMillion", title: t("gpuTime.cards.perMillion"), value: out == null ? DASH : out.toFixed(1),
        sub: out == null ? t("gpuTime.cards.noOutput") : t("gpuTime.cards.perMillionAll", { value: all == null ? DASH : all.toFixed(1) }),
      },
    ];
  };

  const chart = (d: GPUTimeAnalytics) => {
    const labels = d.range.buckets.map((b) => bucketLabel(b, tz, d.range.granularity, spansDays(d.range.buckets, tz)));
    return stackedBarOption({
      labels,
      series: d.series.map((s, i) => ({
        name: s.deployment_name || t("names.deploymentId", { id: s.deployment_id }),
        color: SERIES_COLORS.deployments[i % SERIES_COLORS.deployments.length],
        data: s.gpu_hours.map((h) => Number(h.toFixed(3))),
      })),
      valueFormat: (v) => t("gpuTime.chart.hours", { value: v.toFixed(1) }),
      totalLabel: t("table.total"),
    });
  };

  const columns: ColumnsType<GPUTimeRow> = [
    {
      key: "deployment", title: t("gpuTime.table.deployment"), width: 240,
      sorter: (a, b) => (a.deployment_name || "").localeCompare(b.deployment_name || ""),
      render: (_, r) => <DeploymentCell t={t} deployment={{ id: r.deployment_id, name: r.deployment_name, served: r.served_name, deleted: r.deployment_deleted }} />,
    },
    { key: "model", title: t("gpuTime.table.model"), width: 160, sorter: (a, b) => a.gpu_model.localeCompare(b.gpu_model), render: (_, r) => r.gpu_model || DASH },
    {
      key: "instances", title: t("gpuTime.table.instances"), align: "right", sorter: (a, b) => a.avg_instances - b.avg_instances,
      render: (_, r) => `${r.avg_instances.toFixed(2)} / ${r.max_instances}`,
    },
    { key: "hours", title: t("gpuTime.table.gpuHours"), align: "right", defaultSortOrder: "descend", sorter: (a, b) => a.gpu_hours - b.gpu_hours, render: (_, r) => r.gpu_hours.toFixed(1) },
    { key: "whole", title: t("gpuTime.table.wholeHours"), align: "right", sorter: (a, b) => a.whole_gpu_hours - b.whole_gpu_hours, render: (_, r) => r.whole_gpu_hours.toFixed(1) },
    { key: "prompt", title: t("gpuTime.table.prompt"), align: "right", sorter: (a, b) => a.prompt_tokens - b.prompt_tokens, render: (_, r) => fmtInt(r.prompt_tokens) },
    { key: "output", title: t("gpuTime.table.output"), align: "right", sorter: (a, b) => a.completion_tokens - b.completion_tokens, render: (_, r) => fmtInt(r.completion_tokens) },
    {
      key: "perMillionOutput", title: t("gpuTime.table.perMillionOutput"), align: "right",
      sorter: (a, b) => (a.gpu_hours_per_million_output ?? -1) - (b.gpu_hours_per_million_output ?? -1), render: (_, r) => fmtFixed(r.gpu_hours_per_million_output, 1),
    },
    {
      key: "perMillionAll", title: t("gpuTime.table.perMillionAll"), align: "right",
      sorter: (a, b) => (a.gpu_hours_per_million_tokens ?? -1) - (b.gpu_hours_per_million_tokens ?? -1), render: (_, r) => fmtFixed(r.gpu_hours_per_million_tokens, 1),
    },
  ];

  return (
    <div>
      <Alert type="info" showIcon={false} style={{ marginBottom: 16 }} message={t("gpuTime.note")} />
      <QueryState query={query} isEmpty={(d) => d.rows.length === 0 && d.series.length === 0}>
        {(d) => {
          const hours = d.rows.reduce((a, r) => a + r.gpu_hours, 0);
          const tokens = deploymentTokens(d.rows);
          return (
            <>
              <StatCards items={cards(d)} />
              <Card style={{ marginBottom: 16 }}>
                <Typography.Title level={5} style={{ marginTop: 0 }}>{t("gpuTime.chart.title", { granularity: t(`granularityName.${d.range.granularity}`) })}</Typography.Title>
                <EChart option={chart(d)} height={340} label={t("gpuTime.chart.aria")} />
              </Card>
              <Card styles={{ body: { padding: 0 } }} title={<Space align="baseline"><span>{t("gpuTime.table.title")}</span><Typography.Text type="secondary" style={{ fontSize: 12, fontWeight: 400 }}>{t("gpuTime.table.hint")}</Typography.Text></Space>}>
                <Table<GPUTimeRow>
                  rowKey={(r) => `${r.deployment_id}/${r.gpu_model}`}
                  size="middle"
                  columns={columns}
                  dataSource={d.rows}
                  pagination={d.rows.length > 50 ? { pageSize: 50, showSizeChanger: false } : false}
                  scroll={{ x: 1100 }}
                  locale={{ emptyText: t("state.noRows") }}
                  summary={() => (
                    <Table.Summary fixed>
                      <Table.Summary.Row>
                        <Table.Summary.Cell index={0}><strong>{t("table.total")}</strong></Table.Summary.Cell>
                        <Table.Summary.Cell index={1}>{DASH}</Table.Summary.Cell>
                        <Table.Summary.Cell index={2} align="right">{DASH}</Table.Summary.Cell>
                        <Table.Summary.Cell index={3} align="right">{hours.toFixed(1)}</Table.Summary.Cell>
                        <Table.Summary.Cell index={4} align="right">{d.rows.reduce((a, r) => a + r.whole_gpu_hours, 0).toFixed(1)}</Table.Summary.Cell>
                        <Table.Summary.Cell index={5} align="right">{fmtInt(tokens.prompt)}</Table.Summary.Cell>
                        <Table.Summary.Cell index={6} align="right">{fmtInt(tokens.completion)}</Table.Summary.Cell>
                        <Table.Summary.Cell index={7} align="right">{fmtFixed(perMillion(hours, tokens.completion), 1)}</Table.Summary.Cell>
                        <Table.Summary.Cell index={8} align="right">{fmtFixed(perMillion(hours, tokens.prompt + tokens.completion), 1)}</Table.Summary.Cell>
                      </Table.Summary.Row>
                    </Table.Summary>
                  )}
                />
              </Card>
              <Typography.Paragraph type="secondary" style={{ fontSize: 12, margin: "4px 0 0" }}>
                {t("gpuTime.foot")} {t("zoneNote", { zone: d.range.time_zone || tz })}
              </Typography.Paragraph>
            </>
          );
        }}
      </QueryState>
    </div>
  );
}

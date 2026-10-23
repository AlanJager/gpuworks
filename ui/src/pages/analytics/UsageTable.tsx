import { Table, Typography } from "antd";
import type { ColumnsType } from "antd/es/table";
import { useTranslation } from "react-i18next";
import type { Granularity, UsageCounts } from "../../analyticsApi";
import { DASH, fmtInt } from "./format";
import { DeploymentCell, KeyCell, NameCell, deploymentTitle, keyTitle } from "./names";
import RateBar from "./RateBar";
import { hitRate, uncachedTokens, type UsageGroupBy, type UsageNode } from "./usageModel";
import { shortTime } from "./zoned";

function nodeName(t: ReturnType<typeof useTranslation>["t"], n: UsageNode): string {
  if (n.key) return keyTitle(t, n.key).title;
  if (n.deployment) return deploymentTitle(t, n.deployment);
  return n.model ?? "";
}

function dayLabel(ms: number, tz: string): string {
  return shortTime(ms, tz).slice(0, 5);
}

// UsageTable is the token usage detail: the rows grouped by key, deployment
// or model, each expanding into the next level down, with a totals row.
export default function UsageTable({ tree, totals, group, granularity, tz }: {
  tree: UsageNode[];
  totals: UsageCounts;
  group: UsageGroupBy;
  granularity: Granularity;
  tz: string;
}) {
  const { t } = useTranslation("analytics");
  const span = (n: { firstMs: number; lastMs: number }) => {
    if (!n.lastMs) return DASH;
    const fmt = granularity === "day" ? dayLabel : shortTime;
    return `${fmt(n.firstMs, tz)} → ${fmt(n.lastMs, tz)}`;
  };
  const scope = (n: UsageNode) => {
    if (n.level === "child") return <Typography.Text type="secondary">{DASH}</Typography.Text>;
    if (n.kind === "key") {
      const names = (n.deployments ?? []).map((d) => deploymentTitle(t, d));
      return names.length > 2 ? `${names.slice(0, 2).join(", ")} ${t("usage.table.more", { count: names.length - 2 })}` : names.join(", ");
    }
    if (n.kind === "deployment") return t("usage.table.keys", { count: n.keyCount ?? 0 });
    return t("usage.table.deploymentsOfModel", { count: n.deployments?.length ?? 0, keys: n.keyCount ?? 0 });
  };
  const num = (get: (n: UsageNode) => number | null) => ({
    align: "right" as const,
    sorter: (a: UsageNode, b: UsageNode) => (get(a) ?? -1) - (get(b) ?? -1),
    render: (_: unknown, n: UsageNode) => fmtInt(get(n)),
  });
  const columns: ColumnsType<UsageNode> = [
    {
      key: "name",
      title: t(`usage.table.group.${group}`),
      width: 260,
      sorter: (a, b) => nodeName(t, a).localeCompare(nodeName(t, b)),
      render: (_, n) => {
        if (n.key) return <KeyCell t={t} info={n.key} />;
        if (n.deployment) return <DeploymentCell t={t} deployment={n.deployment} />;
        return <NameCell title={n.model} />;
      },
    },
    { key: "scope", title: t("usage.table.scope"), width: 200, render: (_, n) => scope(n) },
    { key: "requests", title: t("usage.table.requests"), ...num((n) => n.counts.requests) },
    { key: "input", title: t("usage.table.input"), defaultSortOrder: "descend", ...num((n) => n.counts.prompt_tokens) },
    { key: "cached", title: t("usage.table.cached"), ...num((n) => n.counts.cached_tokens) },
    {
      key: "rate",
      title: t("usage.table.hitRate"),
      align: "right",
      sorter: (a, b) => (hitRate(a.counts) ?? -1) - (hitRate(b.counts) ?? -1),
      render: (_, n) => <RateBar percent={hitRate(n.counts)} />,
    },
    { key: "uncached", title: t("usage.table.uncached"), ...num((n) => uncachedTokens(n.counts)) },
    { key: "output", title: t("usage.table.output"), ...num((n) => n.counts.completion_tokens) },
    { key: "reasoning", title: t("usage.table.reasoning"), ...num((n) => n.counts.reasoning_tokens) },
    { key: "noUsage", title: t("usage.table.noUsage"), ...num((n) => n.counts.no_usage) },
    { key: "span", title: t("usage.table.span"), width: 190, render: (_, n) => <span style={{ fontVariantNumeric: "tabular-nums", whiteSpace: "nowrap" }}>{span(n)}</span> },
  ];
  return (
    <Table<UsageNode>
      rowKey="id"
      size="middle"
      columns={columns}
      dataSource={tree}
      pagination={tree.length > 50 ? { pageSize: 50, showSizeChanger: false } : false}
      scroll={{ x: 1280 }}
      locale={{ emptyText: t("state.noRows") }}
      summary={() => (
        <Table.Summary fixed>
          <Table.Summary.Row>
            <Table.Summary.Cell index={0}><strong>{t("table.total")}</strong></Table.Summary.Cell>
            <Table.Summary.Cell index={1}>{DASH}</Table.Summary.Cell>
            <Table.Summary.Cell index={2} align="right">{fmtInt(totals.requests)}</Table.Summary.Cell>
            <Table.Summary.Cell index={3} align="right">{fmtInt(totals.prompt_tokens)}</Table.Summary.Cell>
            <Table.Summary.Cell index={4} align="right">{fmtInt(totals.cached_tokens)}</Table.Summary.Cell>
            <Table.Summary.Cell index={5} align="right"><RateBar percent={hitRate(totals)} /></Table.Summary.Cell>
            <Table.Summary.Cell index={6} align="right">{fmtInt(uncachedTokens(totals))}</Table.Summary.Cell>
            <Table.Summary.Cell index={7} align="right">{fmtInt(totals.completion_tokens)}</Table.Summary.Cell>
            <Table.Summary.Cell index={8} align="right">{fmtInt(totals.reasoning_tokens)}</Table.Summary.Cell>
            <Table.Summary.Cell index={9} align="right">{fmtInt(totals.no_usage)}</Table.Summary.Cell>
            <Table.Summary.Cell index={10}>{DASH}</Table.Summary.Cell>
          </Table.Summary.Row>
        </Table.Summary>
      )}
    />
  );
}

import { useEffect, useMemo, useState } from "react";
import { Button, Card, Segmented, Select, Space, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { APIError } from "../../apiError";
import { analyticsApi, type GpuGroupBy, type GpuMetric, type GPUAnalytics } from "../../analyticsApi";
import { formatBytes } from "../../format";
import { heatHeight, heatmapOption } from "./chartOptions";
import type { CsvModel } from "./csv";
import EChart from "./EChart";
import { useExportModel } from "./exportContext";
import { fmtFixed, fmtInt, fmtPct, fmtWatts } from "./format";
import { filterOptions, groupCards, summarize, IDLE_UTIL, type FilterOption } from "./gpuModel";
import GpuTable from "./GpuTable";
import QueryState from "./QueryState";
import StatCards, { type StatItem } from "./StatCards";
import type { TabProps } from "./types";
import { useAnalyticsData } from "./useAnalyticsData";
import { bucketLabel, spansDays } from "./zoned";

const GROUPS: GpuGroupBy[] = ["model", "node", "gpu"];
const METRICS: GpuMetric[] = ["util", "power", "temp", "mem"];
const TOO_MANY = "ANALYTICS_TOO_MANY_CARDS";

function pick<T extends string>(value: string | null, allowed: T[], fallback: T): T {
  return (allowed as string[]).includes(value ?? "") ? (value as T) : fallback;
}

function metricFormat(metric: GpuMetric): (v: number) => string {
  switch (metric) {
    case "util": return (v) => `${v.toFixed(1)}%`;
    case "power": return (v) => `${Math.round(v)} W`;
    case "temp": return (v) => `${Math.round(v)} °C`;
    case "mem": return (v) => formatBytes(v);
  }
}

export default function GpuTab({ from, to, granularity, tz, reload, search, setSearch }: TabProps) {
  const { t } = useTranslation("analytics");
  const group = pick(search.get("by"), GROUPS, "model");
  const metric = pick(search.get("metric"), METRICS, "util");
  const node = search.get("node") ?? "";
  const model = search.get("model") ?? "";
  const [nodes, setNodes] = useState<FilterOption[]>([]);
  const [models, setModels] = useState<FilterOption[]>([]);

  const query = useAnalyticsData(
    (signal) => analyticsApi.gpus({ from, to, granularity, groupBy: group, metric, node: node || undefined, model: model || undefined }, signal),
    [from, to, granularity, group, metric, node, model, reload],
  );
  const data = query.data;
  const tooMany = query.error instanceof APIError && query.error.code === TOO_MANY;

  // The node and model filters list what the answer without filters holds; a
  // filtered answer only holds the chosen one, so it adds to the lists.
  const adopt = (cards: GPUAnalytics["cards"], replace: boolean) => {
    const found = filterOptions(cards);
    const merge = (old: FilterOption[], next: FilterOption[]) => (replace ? next : [...old, ...next.filter((n) => !old.some((o) => o.value === n.value))]);
    setNodes((old) => merge(old, found.nodes));
    setModels((old) => merge(old, found.models));
  };
  useEffect(() => {
    if (data) adopt(data.cards, !node && !model);
  }, [data]);
  // Asked for every card of a fleet too big for it, the page has no cards to
  // offer the filters from: ask by model, which always fits.
  const needOptions = tooMany && nodes.length === 0;
  useEffect(() => {
    if (!needOptions) return;
    const control = new AbortController();
    analyticsApi.gpus({ from, to, granularity, groupBy: "model", metric: "util" }, control.signal)
      .then((d) => { if (!control.signal.aborted) adopt(d.cards, true); })
      .catch(() => {});
    return () => control.abort();
  }, [needOptions, from, to, granularity]);

  const rows = useMemo(() => (data ? groupCards(data.cards, group) : []), [data, group]);

  const exportModel = useMemo<CsvModel | null>(() => {
    if (!data) return null;
    return {
      name: "gpus",
      head: [
        t(`gpus.table.unit.${group}`), t("gpus.export.detail"), t("gpus.table.utilAvg"), t("gpus.table.utilMax"), t("gpus.export.memAvgGiB"), t("gpus.export.memAvgPct"),
        t("gpus.export.memMaxGiB"), t("gpus.export.memMaxPct"), group === "gpu" ? t("gpus.table.power") : t("gpus.table.powerPerCard"), t("gpus.table.tempMax"), t("gpus.table.deployments"), t("gpus.table.idle"),
      ],
      rows: rows.map((r) => [
        r.title, r.sub, fmtFixed(r.utilAvg, 1), fmtFixed(r.utilMax, 1),
        r.memAvg == null ? "" : (r.memAvg / 1024 ** 3).toFixed(1), r.memAvgPercent == null ? "" : r.memAvgPercent.toFixed(1),
        r.memMax == null ? "" : (r.memMax / 1024 ** 3).toFixed(1), r.memMaxPercent == null ? "" : r.memMaxPercent.toFixed(1),
        r.powerAvg == null ? "" : Math.round(r.powerAvg), r.tempMax == null ? "" : Math.round(r.tempMax),
        r.deployments.map((d) => d.name).join(" / ") || t("gpus.table.unassigned"), r.utilAvg != null && r.utilAvg < IDLE_UTIL ? t("gpus.export.yes") : "",
      ]),
    };
  }, [data, rows, group, t]);
  useExportModel(exportModel);

  const cards = (d: GPUAnalytics): StatItem[] => {
    const s = summarize(d.cards, d.totals);
    return [
      { key: "cards", title: t("gpus.cards.cards"), value: fmtInt(s.cards), sub: s.byModel.map((m) => `${m.model || "?"} × ${m.count}`).join(" · ") || undefined },
      { key: "util", title: t("gpus.cards.util"), value: fmtPct(s.utilAvg), sub: t("gpus.cards.utilMax", { value: fmtPct(s.utilMax) }) },
      {
        key: "mem", title: t("gpus.cards.mem"), value: fmtPct(s.memPercentAvg),
        sub: s.memUsedAvg == null ? undefined : t("gpus.cards.memPerCard", { value: formatBytes(s.memUsedAvg) }),
      },
      {
        key: "power", title: t("gpus.cards.power"), value: s.powerTotal == null ? "—" : s.powerTotal >= 1000 ? `${(s.powerTotal / 1000).toFixed(2)} kW` : fmtWatts(s.powerTotal),
        sub: s.powerTotal == null ? undefined : t("gpus.cards.powerPerCard", { value: fmtWatts(s.powerTotal / s.powerCards) }),
      },
      { key: "idle", title: t("gpus.cards.idle"), value: `${s.idle} / ${s.measured}`, tip: t("gpus.cards.idleTip", { threshold: IDLE_UTIL }), sub: t("gpus.cards.idleSub", { threshold: IDLE_UTIL }) },
    ];
  };

  const heat = (d: GPUAnalytics) => {
    const labels = d.range.buckets.map((b) => bucketLabel(b, tz, d.range.granularity, spansDays(d.range.buckets, tz)));
    return heatmapOption({ labels, rows: d.heat, metric: d.metric, valueFormat: metricFormat(d.metric) });
  };

  const tooManyHelp = () => (
    <div style={{ marginTop: 8 }}>
      <div>{t("gpus.tooMany.hint")}</div>
      <Space style={{ marginTop: 8 }} wrap>
        <Button size="small" onClick={() => setSearch({ by: "model" })}>{t("gpus.tooMany.byModel")}</Button>
        <Button size="small" onClick={() => setSearch({ by: "node" })}>{t("gpus.tooMany.byNode")}</Button>
      </Space>
    </div>
  );

  return (
    <div>
      <Space wrap size={16} style={{ marginBottom: 16 }}>
        <Space size={8}>
          <Typography.Text type="secondary">{t("gpus.filters.group")}</Typography.Text>
          <Segmented value={group} onChange={(v) => setSearch({ by: v === "model" ? undefined : String(v) })} options={GROUPS.map((g) => ({ value: g, label: t(`gpus.group.${g}`) }))} />
        </Space>
        <Space size={8}>
          <Typography.Text type="secondary">{t("gpus.filters.metric")}</Typography.Text>
          <Segmented value={metric} onChange={(v) => setSearch({ metric: v === "util" ? undefined : String(v) })} options={METRICS.map((m) => ({ value: m, label: t(`gpus.metric.${m}`) }))} />
        </Space>
        <Space size={8}>
          <Typography.Text type="secondary">{t("gpus.filters.node")}</Typography.Text>
          <Select
            allowClear
            showSearch
            optionFilterProp="label"
            style={{ minWidth: 180 }}
            aria-label={t("gpus.filters.node")}
            placeholder={t("gpus.filters.allNodes")}
            value={node || undefined}
            options={nodes}
            onChange={(v) => setSearch({ node: v || undefined })}
          />
        </Space>
        <Space size={8}>
          <Typography.Text type="secondary">{t("gpus.filters.model")}</Typography.Text>
          <Select
            allowClear
            showSearch
            optionFilterProp="label"
            style={{ minWidth: 180 }}
            aria-label={t("gpus.filters.model")}
            placeholder={t("gpus.filters.allModels")}
            value={model || undefined}
            options={models}
            onChange={(v) => setSearch({ model: v || undefined })}
          />
        </Space>
      </Space>
      <QueryState query={query} isEmpty={(d) => d.cards.length === 0} errorExtra={() => (tooMany ? tooManyHelp() : null)}>
        {(d) => (
          <>
            <StatCards items={cards(d)} />
            <Card style={{ marginBottom: 16 }}>
              <Typography.Title level={5} style={{ marginTop: 0 }}>
                {t(`gpus.chart.title.${d.metric}`, { granularity: t(`granularityName.${d.range.granularity}`) })}
              </Typography.Title>
              <EChart option={heat(d)} height={heatHeight(d.heat.length)} label={t("gpus.chart.aria")} />
            </Card>
            <Card styles={{ body: { padding: 0 } }} title={<Space align="baseline"><span>{t(`gpus.table.title.${group}`)}</span><Typography.Text type="secondary" style={{ fontSize: 12, fontWeight: 400 }}>{t("gpus.table.hint", { threshold: IDLE_UTIL })}</Typography.Text></Space>}>
              <GpuTable rows={rows} group={group} />
            </Card>
            <Typography.Paragraph type="secondary" style={{ fontSize: 12, margin: "4px 0 0" }}>
              {t("gpus.foot")} {t("zoneNote", { zone: d.range.time_zone || tz })}
            </Typography.Paragraph>
          </>
        )}
      </QueryState>
    </div>
  );
}

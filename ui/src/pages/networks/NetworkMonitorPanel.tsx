import { useEffect, useMemo, useState } from "react";
import { Alert, Card, Col, Row, Segmented, Space, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { api, type MetricPoint, type MetricSeries } from "../../api";
import type { NetworkDetail, NetworkNode } from "../../networkTypes";
import TimeSeriesChart from "../../components/TimeSeriesChart";
import { formatBitRate, isAcceleratorRDMA, isHostRDMA } from "./networkDisplay";

const RANGES = ["15m", "1h", "6h", "24h"] as const;
type Range = (typeof RANGES)[number];
const STEPS: Record<Range, string> = { "15m": "10s", "1h": "30s", "6h": "2m", "24h": "10m" };

const METRICS = [
  "gpuworks_node_nic_rx_bytes_per_sec", "gpuworks_node_nic_tx_bytes_per_sec", "gpuworks_node_nic_errors_per_sec",
  "gpuworks_node_rdma_rx_bytes_per_sec", "gpuworks_node_rdma_tx_bytes_per_sec", "gpuworks_node_rdma_events_per_sec",
  "gpuworks_node_role_rx_bytes_per_sec", "gpuworks_node_role_tx_bytes_per_sec",
] as const;
type Metric = (typeof METRICS)[number];

/** Adds series point by point; points at the same time are summed. */
export function sumSeries(series: MetricSeries[]): MetricPoint[] {
  const byTime = new Map<number, number>();
  for (const s of series) for (const p of s.points) byTime.set(p[0], (byTime.get(p[0]) ?? 0) + p[1]);
  return [...byTime.entries()].sort((a, b) => a[0] - b[0]).map(([time, value]): MetricPoint => [time, value]);
}

/**
 * One line per node for what the network carries there: the node's
 * interface on it, its HCAs for an RDMA network, and the node's traffic for
 * the roles the network has. Series are queried once per metric for the
 * whole cluster and picked out here, so the page costs a fixed number of
 * requests however many nodes the network has.
 */
export function perNode(nodes: NetworkNode[], series: MetricSeries[] | undefined, pick: (n: NetworkNode, labels: Record<string, string>) => boolean): MetricSeries[] {
  if (!series) return [];
  return nodes
    .map((n) => ({ labels: { node: n.node_name }, points: sumSeries(series.filter((s) => s.labels.node === n.node_uuid && pick(n, s.labels))) }))
    .filter((s) => s.points.length > 0);
}

export default function NetworkMonitorPanel({ network }: { network: NetworkDetail }) {
  const { t } = useTranslation("networks");
  const [range, setRange] = useState<Range>("1h");
  const [data, setData] = useState<Partial<Record<Metric, MetricSeries[]>> | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    setData(null);
    Promise.all(METRICS.map(async (metric) => [metric, (await api.metricsQuery(metric, { range, step: STEPS[range] })).items] as const))
      .then((pairs) => { if (alive) { setData(Object.fromEntries(pairs)); setError(null); } })
      .catch((e: Error) => { if (alive) setError(e.message); });
    return () => { alive = false; };
  }, [range]);

  const nodes = network.nodes;
  const onIface = (n: NetworkNode, l: Record<string, string>) => !!n.interface && l.iface === n.interface;
  const onHCA = (n: NetworkNode, l: Record<string, string>) => n.hcas.some((h) => h.name === l.hca);
  const charts = useMemo(() => {
    if (!data) return [];
    const out: { key: string; title: string; series: MetricSeries[]; rate: boolean; hint?: string }[] = [
      { key: "nic_rx", title: t("monitor.nicRx"), series: perNode(nodes, data.gpuworks_node_nic_rx_bytes_per_sec, onIface), rate: true },
      { key: "nic_tx", title: t("monitor.nicTx"), series: perNode(nodes, data.gpuworks_node_nic_tx_bytes_per_sec, onIface), rate: true },
    ];
    if (isHostRDMA(network.type)) {
      out.push(
        { key: "rdma_rx", title: t("monitor.rdmaRx"), series: perNode(nodes, data.gpuworks_node_rdma_rx_bytes_per_sec, onHCA), rate: true },
        { key: "rdma_tx", title: t("monitor.rdmaTx"), series: perNode(nodes, data.gpuworks_node_rdma_tx_bytes_per_sec, onHCA), rate: true },
      );
    }
    const errors = [
      ...perNode(nodes, data.gpuworks_node_nic_errors_per_sec, onIface),
      ...(isHostRDMA(network.type) ? perNode(nodes, data.gpuworks_node_rdma_events_per_sec, onHCA) : []),
    ];
    const byNode = new Map<string, MetricSeries[]>();
    for (const s of errors) byNode.set(s.labels.node, [...(byNode.get(s.labels.node) ?? []), s]);
    out.push({
      key: "errors", title: t("monitor.errors"), rate: false, hint: t("monitor.errorsHint"),
      series: [...byNode.entries()].map(([node, list]) => ({ labels: { node }, points: sumSeries(list) })),
    });
    for (const role of ["storage_sync", "serving"] as const) {
      if (!network.roles.includes(role)) continue;
      const ofRole = (_: NetworkNode, l: Record<string, string>) => l.role === role;
      const both = [...(data.gpuworks_node_role_rx_bytes_per_sec ?? []), ...(data.gpuworks_node_role_tx_bytes_per_sec ?? [])];
      out.push({ key: `role_${role}`, title: t(`monitor.role.${role}`), series: perNode(nodes, both, ofRole), rate: true, hint: t("monitor.roleHint") });
    }
    return out;
  }, [data, nodes, network.type, network.roles, t]);

  return (
    <Space direction="vertical" size={16} style={{ width: "100%" }}>
      <Space wrap>
        <Segmented value={range} onChange={(v) => setRange(v as Range)} options={[...RANGES]} size="small" />
        <Typography.Text type="secondary">{t("monitor.scope")}</Typography.Text>
      </Space>
      {error && <Alert type="error" showIcon message={t("monitor.loadFailed", { detail: error })} />}
      {nodes.length === 0 ? (
        <Typography.Text type="secondary">{t("detail.noNodes")}</Typography.Text>
      ) : (
        <Row gutter={[16, 16]}>
          {charts.map((chart) => (
            <Col xs={24} xl={12} key={chart.key}>
              <Card size="small" title={chart.title} extra={chart.hint && <Typography.Text type="secondary" style={{ fontWeight: 400 }}>{chart.hint}</Typography.Text>}>
                <TimeSeriesChart
                  height={200}
                  series={chart.series}
                  valueFmt={chart.rate ? formatBitRate : (v) => `${v.toFixed(v < 10 ? 1 : 0)}/s`}
                  seriesName={(labels) => labels.node}
                  group={`network-${network.id}`}
                />
              </Card>
            </Col>
          ))}
          {!data && !error && <Col span={24}><Card size="small" loading /></Col>}
        </Row>
      )}
      {network.type === "host_ethernet" && network.roles.includes("compute") && (
        <Alert type="info" showIcon message={t("monitor.computeEthernet")} />
      )}
      {isAcceleratorRDMA(network.type) && (
        <Alert type="info" showIcon message={t("monitor.acceleratorNote")} />
      )}
    </Space>
  );
}

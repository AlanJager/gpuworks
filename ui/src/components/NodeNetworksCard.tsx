import { useEffect, useState } from "react";
import { networkDetailPath } from "../pages/networks/networkLinks";
import { Card, Descriptions, Empty, Space, Table, Tag, Tooltip, Typography } from "antd";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import type { AcceleratorPort, NetInterface, NodeLoadedNetwork, NodeNetworks, RDMAPort, ServingForward } from "../networkTypes";
import { formatTime } from "../i18n/datetime";
import { Endpoint, HCAList, LinkRate, RoleTags, TypeTag, WarningMark, networkName, sourceText } from "../pages/networks/networkDisplay";
import NodeManagementCard from "./NodeManagementCard";

function speed(mbps: number) {
  if (!mbps) return "—";
  return mbps >= 1000 ? `${mbps / 1000}G` : `${mbps}M`;
}

/** Networks a node loaded, plus the inventory its agent reported. */
export default function NodeNetworksCard({ nodeUUID }: { nodeUUID: string }) {
  const { t } = useTranslation("networks");
  const [data, setData] = useState<NodeNetworks | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    api.nodeNetworks(nodeUUID)
      .then((d) => { if (!cancelled) { setData(d); setError(null); } })
      .catch((e: Error) => { if (!cancelled) setError(e.message); });
    return () => { cancelled = true; };
  }, [nodeUUID]);

  if (error) return <Card size="small" title={t("node.loaded")}><Typography.Text type="danger">{error}</Typography.Text></Card>;
  // The inventory names its tables only when there is more than one; a lone
  // interface table under the card's own title needs no heading of its own.
  const inventoryTables = 1 + ((data?.rdma_ports.length ?? 0) > 0 ? 1 : 0) + ((data?.accelerator_ports?.length ?? 0) > 0 ? 1 : 0);

  return (
    <Space direction="vertical" size={16} style={{ width: "100%", display: "flex" }}>
      <Card size="small" title={t("node.loaded")} loading={!data}>
        {data && data.networks.length === 0 ? (
          <Typography.Text type="secondary">{t("node.none")}</Typography.Text>
        ) : (
          <Table<NodeLoadedNetwork>
            rowKey="role"
            size="small"
            pagination={false}
            dataSource={data?.networks ?? []}
            scroll={{ x: "max-content" }}
            columns={[
              { title: t("table.roles"), dataIndex: "role", render: (_, n) => <RoleTags roles={[n.role]} /> },
              {
                title: t("table.name"),
                dataIndex: "network_name",
                render: (_, n) => (
                  <span>
                    <Link to={networkDetailPath(n.network_id)}>{networkName(t, { name: n.network_name, builtin: n.source === "management" })}</Link>
                    <WarningMark warnings={n.warnings} />
                  </span>
                ),
              },
              { title: t("table.type"), dataIndex: "network_type", render: (_, n) => <TypeTag type={n.network_type} /> },
              {
                title: t("nodeTable.source"),
                dataIndex: "source",
                render: (_, n) => sourceText(t, n),
              },
              { title: t("nodeTable.endpoint"), key: "endpoint", render: (_, n) => <Endpoint iface={n.interface} ip={n.ip} /> },
              { title: t("nodeTable.hcas"), key: "hcas", render: (_, n) => <HCAList hcas={n.hcas} linkLayer={n.link_layer} /> },
            ]}
          />
        )}
      </Card>
      {data?.management && <NodeManagementCard nodeUUID={nodeUUID} management={data.management} onChanged={setData} />}
      {data?.serving_forward && <ServingForwardCard forward={data.serving_forward} />}
      <Card size="small" title={<span>{t("node.inventory")} <Typography.Text type="secondary" style={{ fontWeight: 400 }}>{t("node.inventoryHint")}</Typography.Text></span>} loading={!data}>
        {data && !data.inventory_reported ? (
          <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t("node.notReported")} />
        ) : (
          // A block, not antd's inline-flex: an inline box sits on a line box's
          // baseline and opened a gap above the first table.
          <Space direction="vertical" size={12} style={{ width: "100%", display: "flex" }}>
            <Table<NetInterface>
              rowKey="name"
              size="small"
              pagination={false}
              title={inventoryTables > 1 ? () => t("node.interfaces") : undefined}
              dataSource={data?.interfaces ?? []}
              scroll={{ x: "max-content" }}
              columns={[
                { title: t("node.iface.name"), dataIndex: "name", render: (v: string) => <Typography.Text code>{v}</Typography.Text> },
                { title: t("node.iface.addrs"), dataIndex: "addrs", render: (v: string[]) => v.length ? v.join(", ") : "—" },
                { title: t("node.iface.state"), dataIndex: "state", render: (v: string) => <Tag color={v === "up" ? "green" : "default"}>{v || "—"}</Tag> },
                { title: t("node.iface.speed"), dataIndex: "speed_mbps", render: (v: number) => speed(v) },
                { title: t("node.iface.mtu"), dataIndex: "mtu", render: (v: number) => v || "—" },
                { title: t("node.rate"), key: "rate", render: (_, i) => <LinkRate rx={i.rx_bytes_per_sec} tx={i.tx_bytes_per_sec} /> },
              ]}
            />
            {(data?.rdma_ports.length ?? 0) > 0 && (
              <Table<RDMAPort>
                rowKey={(p) => `${p.device}:${p.port}`}
                size="small"
                pagination={false}
                title={() => t("node.rdma")}
                dataSource={data?.rdma_ports ?? []}
                scroll={{ x: "max-content" }}
                columns={[
                  { title: t("node.port.device"), key: "device", render: (_, p) => <Typography.Text code>{`${p.device}:${p.port}`}</Typography.Text> },
                  { title: t("node.port.state"), dataIndex: "state", render: (v: string) => <Tag color={v === "ACTIVE" ? "green" : "default"}>{v || "—"}</Tag> },
                  { title: t("node.port.linkLayer"), dataIndex: "link_layer" },
                  { title: t("node.port.rate"), dataIndex: "rate_gbps", render: (v: number) => v ? `${v}G` : "—" },
                  { title: t("node.port.netdev"), dataIndex: "netdev", render: (v: string) => v || "—" },
                  { title: t("node.port.ip"), dataIndex: "ip", render: (v: string) => v || "—" },
                  { title: t("node.rate"), key: "rate", render: (_, p) => <LinkRate rx={p.rx_bytes_per_sec} tx={p.tx_bytes_per_sec} /> },
                ]}
              />
            )}
            {(data?.accelerator_ports?.length ?? 0) > 0 && (
              <Table<AcceleratorPort>
                rowKey="name"
                size="small"
                pagination={false}
                title={() => <span>{t("node.accelerator")} <Typography.Text type="secondary" style={{ fontWeight: 400 }}>{t("node.acceleratorHint")}</Typography.Text></span>}
                dataSource={data?.accelerator_ports ?? []}
                scroll={{ x: "max-content" }}
                columns={[
                  { title: t("node.card.name"), dataIndex: "name", render: (v: string) => <Typography.Text code>{v}</Typography.Text> },
                  { title: t("node.card.ip"), key: "ip", render: (_, p) => p.ip ? `${p.ip}${p.netmask ? ` / ${p.netmask}` : ""}` : "—" },
                  { title: t("node.card.state"), dataIndex: "state", render: (v: string) => <Tag color={v === "UP" ? "green" : "default"}>{v || "—"}</Tag> },
                  { title: t("node.card.speed"), dataIndex: "speed_gbps", render: (v: number) => v ? `${v}G` : "—" },
                ]}
              />
            )}
          </Space>
        )}
      </Card>
    </Space>
  );
}

/** Whether the gateway reaches this node's instances over its serving network. */
function ServingForwardCard({ forward }: { forward: ServingForward }) {
  const { t } = useTranslation("networks");
  let status = <Tag>{t("node.forward.unused")}</Tag>;
  if (forward.mode === "direct") status = <Tag color="green">{t("node.forward.direct")}</Tag>;
  if (forward.mode === "unreachable") {
    status = (
      <Tooltip trigger={["hover", "focus"]} title={forward.reason}>
        <Tag tabIndex={0} color="red" aria-label={`${t("node.forward.unreachable")}: ${forward.reason ?? ""}`}>{t("node.forward.unreachable")}</Tag>
      </Tooltip>
    );
  }
  return (
    <Card size="small" title={t("node.forward.title")}>
      <Descriptions column={1} size="small">
        <Descriptions.Item label={t("node.forward.address")}><Typography.Text code>{forward.address}</Typography.Text></Descriptions.Item>
        <Descriptions.Item label={t("node.forward.status")}>
          <Space>{status}{forward.at && <Typography.Text type="secondary">{formatTime(forward.at)}</Typography.Text>}</Space>
        </Descriptions.Item>
      </Descriptions>
      <Typography.Text type="secondary">{t("node.forward.hint")}</Typography.Text>
    </Card>
  );
}

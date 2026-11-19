import { useEffect, useState } from "react";
import { networkDetailPath } from "../pages/networks/networkLinks";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import type { AcceleratorPort, NetInterface, NodeLoadedNetwork, NodeNetworks, RDMAPort, ServingForward } from "../networkTypes";
import { formatTime } from "../i18n/datetime";
import { Endpoint, HCAList, LinkRate, RoleTags, TypeTag, WarningMark, networkName, sourceText } from "../pages/networks/networkDisplay";
import NodeManagementCard from "./NodeManagementCard";
import EmptyState from "./EmptyState";
import { Badge } from "./ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "./ui/card";
import { DetailList, DetailListItem, DetailListLabel, DetailListValue } from "./ui/detail-list";
import { Skeleton } from "./ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "./ui/table";
import { Tooltip } from "./ui/tooltip";

const CODE = "rounded-sm bg-emphasis px-1 font-mono text-label";

interface FactColumn<T> { key: string; title: string; render: (row: T) => ReactNode }

/** A small fact table: every row shown, no paging or sorting, scrolling sideways when wide. */
function FactTable<T>({ caption, rows, rowKey, columns }: { caption?: ReactNode; rows: T[]; rowKey: (row: T) => string; columns: FactColumn<T>[] }) {
  return (
    <div className="flex min-w-0 flex-col gap-1">
      {caption && <div className="font-medium">{caption}</div>}
      <Table size="md" containerClassName="overflow-x-auto">
        <TableHeader>
          <TableRow>{columns.map((c) => <TableHead key={c.key} className="whitespace-nowrap">{c.title}</TableHead>)}</TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((row) => (
            <TableRow key={rowKey(row)}>{columns.map((c) => <TableCell key={c.key} className="whitespace-nowrap">{c.render(row)}</TableCell>)}</TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

function Panel({ title, loading, children }: { title: ReactNode; loading?: boolean; children?: ReactNode }) {
  return (
    <Card className="border border-border-subtle" aria-busy={loading || undefined}>
      <CardHeader><CardTitle>{title}</CardTitle></CardHeader>
      <CardContent className="pb-4">
        {loading ? <div className="flex flex-col gap-2"><Skeleton className="h-4 w-2/5" /><Skeleton className="h-4 w-full" /><Skeleton className="h-4 w-full" /></div> : children}
      </CardContent>
    </Card>
  );
}

function state(value: string, up: string) {
  return <Badge size="sm" status={value === up ? "success" : "neutral"}>{value || "—"}</Badge>;
}

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

  if (error) return <Panel title={t("node.loaded")}><span className="text-fg-danger">{error}</span></Panel>;
  // The inventory names its tables only when there is more than one; a lone
  // interface table under the card's own title needs no heading of its own.
  const inventoryTables = 1 + ((data?.rdma_ports.length ?? 0) > 0 ? 1 : 0) + ((data?.accelerator_ports?.length ?? 0) > 0 ? 1 : 0);

  return (
    <div className="flex w-full flex-col gap-4">
      <Panel title={t("node.loaded")} loading={!data}>
        {data && data.networks.length === 0 ? (
          <span className="text-fg-muted">{t("node.none")}</span>
        ) : (
          <FactTable<NodeLoadedNetwork>
            rowKey={(n) => n.role}
            rows={data?.networks ?? []}
            columns={[
              { key: "role", title: t("table.roles"), render: (n) => <RoleTags roles={[n.role]} /> },
              {
                key: "name",
                title: t("table.name"),
                render: (n) => (
                  <span>
                    <Link to={networkDetailPath(n.network_id)}>{networkName(t, { name: n.network_name, builtin: n.source === "management" })}</Link>
                    <WarningMark warnings={n.warnings} />
                  </span>
                ),
              },
              { key: "type", title: t("table.type"), render: (n) => <TypeTag type={n.network_type} /> },
              { key: "source", title: t("nodeTable.source"), render: (n) => sourceText(t, n) },
              { key: "endpoint", title: t("nodeTable.endpoint"), render: (n) => <Endpoint iface={n.interface} ip={n.ip} /> },
              { key: "hcas", title: t("nodeTable.hcas"), render: (n) => <HCAList hcas={n.hcas} linkLayer={n.link_layer} /> },
            ]}
          />
        )}
      </Panel>
      {data?.management && <NodeManagementCard nodeUUID={nodeUUID} management={data.management} onChanged={setData} />}
      {data?.serving_forward && <ServingForwardCard forward={data.serving_forward} />}
      <Panel title={<span>{t("node.inventory")} <span className="font-normal text-fg-muted">{t("node.inventoryHint")}</span></span>} loading={!data}>
        {data && !data.inventory_reported ? (
          <EmptyState compact description={t("node.notReported")} />
        ) : (
          <div className="flex w-full flex-col gap-3">
            <FactTable<NetInterface>
              rowKey={(i) => i.name}
              caption={inventoryTables > 1 ? t("node.interfaces") : undefined}
              rows={data?.interfaces ?? []}
              columns={[
                { key: "name", title: t("node.iface.name"), render: (i) => <code className={CODE}>{i.name}</code> },
                { key: "addrs", title: t("node.iface.addrs"), render: (i) => i.addrs.length ? i.addrs.join(", ") : "—" },
                { key: "state", title: t("node.iface.state"), render: (i) => state(i.state, "up") },
                { key: "speed", title: t("node.iface.speed"), render: (i) => speed(i.speed_mbps) },
                { key: "mtu", title: t("node.iface.mtu"), render: (i) => i.mtu || "—" },
                { key: "rate", title: t("node.rate"), render: (i) => <LinkRate rx={i.rx_bytes_per_sec} tx={i.tx_bytes_per_sec} /> },
              ]}
            />
            {(data?.rdma_ports.length ?? 0) > 0 && (
              <FactTable<RDMAPort>
                rowKey={(p) => `${p.device}:${p.port}`}
                caption={t("node.rdma")}
                rows={data?.rdma_ports ?? []}
                columns={[
                  { key: "device", title: t("node.port.device"), render: (p) => <code className={CODE}>{`${p.device}:${p.port}`}</code> },
                  { key: "state", title: t("node.port.state"), render: (p) => state(p.state, "ACTIVE") },
                  { key: "link_layer", title: t("node.port.linkLayer"), render: (p) => p.link_layer },
                  { key: "rate_gbps", title: t("node.port.rate"), render: (p) => p.rate_gbps ? `${p.rate_gbps}G` : "—" },
                  { key: "netdev", title: t("node.port.netdev"), render: (p) => p.netdev || "—" },
                  { key: "ip", title: t("node.port.ip"), render: (p) => p.ip || "—" },
                  { key: "rate", title: t("node.rate"), render: (p) => <LinkRate rx={p.rx_bytes_per_sec} tx={p.tx_bytes_per_sec} /> },
                ]}
              />
            )}
            {(data?.accelerator_ports?.length ?? 0) > 0 && (
              <FactTable<AcceleratorPort>
                rowKey={(p) => p.name}
                caption={<span>{t("node.accelerator")} <span className="font-normal text-fg-muted">{t("node.acceleratorHint")}</span></span>}
                rows={data?.accelerator_ports ?? []}
                columns={[
                  { key: "name", title: t("node.card.name"), render: (p) => <code className={CODE}>{p.name}</code> },
                  { key: "ip", title: t("node.card.ip"), render: (p) => p.ip ? `${p.ip}${p.netmask ? ` / ${p.netmask}` : ""}` : "—" },
                  { key: "state", title: t("node.card.state"), render: (p) => state(p.state, "UP") },
                  { key: "speed", title: t("node.card.speed"), render: (p) => p.speed_gbps ? `${p.speed_gbps}G` : "—" },
                ]}
              />
            )}
          </div>
        )}
      </Panel>
    </div>
  );
}

/** Whether the gateway reaches this node's instances over its serving network. */
function ServingForwardCard({ forward }: { forward: ServingForward }) {
  const { t } = useTranslation("networks");
  let status = <Badge size="sm" status="neutral">{t("node.forward.unused")}</Badge>;
  if (forward.mode === "direct") status = <Badge size="sm" status="success">{t("node.forward.direct")}</Badge>;
  if (forward.mode === "unreachable") {
    status = (
      <Tooltip content={forward.reason}>
        <Badge size="sm" tabIndex={0} status="danger" aria-label={`${t("node.forward.unreachable")}: ${forward.reason ?? ""}`}>{t("node.forward.unreachable")}</Badge>
      </Tooltip>
    );
  }
  return (
    <Panel title={t("node.forward.title")}>
      <div className="flex flex-col gap-2">
        <DetailList>
          <DetailListItem>
            <DetailListLabel>{t("node.forward.address")}</DetailListLabel>
            <DetailListValue><code className={CODE}>{forward.address}</code></DetailListValue>
          </DetailListItem>
          <DetailListItem>
            <DetailListLabel>{t("node.forward.status")}</DetailListLabel>
            <DetailListValue><span className="inline-flex flex-wrap items-center justify-end gap-2">{status}{forward.at && <span className="text-fg-muted">{formatTime(forward.at)}</span>}</span></DetailListValue>
          </DetailListItem>
        </DetailList>
        <span className="text-fg-muted">{t("node.forward.hint")}</span>
      </div>
    </Panel>
  );
}

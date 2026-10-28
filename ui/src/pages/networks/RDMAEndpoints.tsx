import { useState } from "react";
import { Button, Checkbox, Popconfirm, Select, Space, Tag, Tooltip, Typography, message } from "antd";
import { CloseOutlined, ExclamationCircleFilled, PlusOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import type { NetworkDetail, NetworkNode, NetworkRail, NodeNetworks, RailGap } from "../../networkTypes";
import { colors } from "../../theme/tokens";
import { isAcceleratorRDMA } from "./networkDisplay";

type T = (key: string, options?: Record<string, unknown>) => string;

/** Why a port cannot carry RDMA traffic, in the viewer's language. */
export function railGapText(t: T, gap?: RailGap | string) {
  return gap ? t(`rail.gap.${gap}`, { defaultValue: gap }) : "";
}

/** "auto" or "manual", with a way back to auto when manual. */
function SourceTag({ source, onReset, busy }: { source?: string; onReset?: () => void; busy?: boolean }) {
  const { t } = useTranslation("networks");
  if (source !== "manual") return <Tag>{t("rail.auto")}</Tag>;
  return (
    <Space size={0}>
      <Tag color="blue">{t("rail.manual")}</Tag>
      {onReset && <Button type="link" size="small" loading={busy} onClick={onReset}>{t("rail.reset")}</Button>}
    </Space>
  );
}

/**
 * Removes an HCA, from every node that uses it (the default: rails pair up
 * across nodes, so they stay alike) or from one node only.
 */
export function RemoveHCAButton({ network, hca, nodeUUID, nodeName, onChanged, label }: {
  network: NetworkDetail;
  hca: string;
  nodeUUID: string;
  nodeName: string;
  onChanged: (detail: NetworkDetail) => void;
  label?: string;
}) {
  const { t } = useTranslation("networks");
  const [onlyThis, setOnlyThis] = useState(false);
  const remove = async () => {
    try {
      const detail = await api.editNetworkHCAs(network.id, { op: "remove", hca, node_uuid: onlyThis ? nodeUUID : undefined });
      message.success(t("rail.removed"));
      onChanged(detail);
    } catch (e) {
      message.error(t("message.failed", { detail: (e as Error).message }));
    }
  };
  return (
    <Popconfirm
      title={t("rail.removeTitle", { hca })}
      description={<Checkbox checked={onlyThis} onChange={(e) => setOnlyThis(e.target.checked)}>{t("rail.onlyNode", { node: nodeName })}</Checkbox>}
      onConfirm={remove}
      onOpenChange={(open) => { if (open) setOnlyThis(false); }}
      okText={t("rail.confirm")}
      cancelText={t("actions.cancel")}
    >
      {label
        ? <Button size="small" danger>{label}</Button>
        : <Button type="text" size="small" danger icon={<CloseOutlined />} aria-label={t("rail.remove", { hca })} />}
    </Popconfirm>
  );
}

/** Loads a node's inventory on first use, for picking a port or an interface. */
function useInventory(nodeUUID: string) {
  const [inventory, setInventory] = useState<NodeNetworks | null>(null);
  const [loading, setLoading] = useState(false);
  const load = () => {
    if (inventory || loading) return;
    setLoading(true);
    api.nodeNetworks(nodeUUID).then(setInventory).catch(() => undefined).finally(() => setLoading(false));
  };
  return { inventory, loading, load };
}

/** An RDMA network's rails on one node: each with its address, removable, and a way to add one. */
export function RailsCell({ network, node, onChanged }: { network: NetworkDetail; node: NetworkNode; onChanged: (detail: NetworkDetail) => void }) {
  const { t } = useTranslation("networks");
  const { inventory, loading, load } = useInventory(node.node_uuid);
  const [adding, setAdding] = useState(false);
  const [busy, setBusy] = useState(false);
  const used = new Set(node.hcas.map((h) => h.name));
  const edit = async (body: Parameters<typeof api.editNetworkHCAs>[1]) => {
    setBusy(true);
    try {
      onChanged(await api.editNetworkHCAs(network.id, body));
      setAdding(false);
    } catch (e) {
      message.error(t("message.failed", { detail: (e as Error).message }));
    } finally {
      setBusy(false);
    }
  };
  // An accelerator network's rails are the cards; a host RDMA network's, the host's RDMA ports.
  const accelerator = isAcceleratorRDMA(network.type);
  const hccs = network.type === "accelerator_hccs";
  const ports: { name: string; ip: string; gap?: string }[] = accelerator
    ? (inventory?.accelerator_ports ?? []).map((p) => ({ name: p.name, ip: hccs ? (p.hccs_ip ?? "") : p.ip, gap: hccs ? p.hccs_gap : p.gap }))
    : (inventory?.rdma_ports ?? []).map((p) => ({ name: `${p.device}:${p.port}`, ip: p.ip, gap: p.gap }));
  const options = ports
    .filter((p) => !used.has(p.name))
    .map((p) => ({
      value: p.name,
      label: [p.name, p.ip || "—", p.gap ? railGapText(t, p.gap) : ""].filter(Boolean).join(" · "),
    }));
  return (
    <Space direction="vertical" size={2}>
      {node.hcas.length === 0 && <Typography.Text type="secondary">—</Typography.Text>}
      {node.hcas.map((rail) => (
        <RailLine key={rail.name} rail={rail}>
          <RemoveHCAButton network={network} hca={rail.name} nodeUUID={node.node_uuid} nodeName={node.node_name} onChanged={onChanged} />
        </RailLine>
      ))}
      <Space size={4} wrap>
        {adding ? (
          <Select
            autoFocus
            size="small"
            style={{ minWidth: 260 }}
            open
            loading={loading}
            placeholder={t(accelerator ? "rail.pickCard" : "rail.pick")}
            options={options}
            notFoundContent={loading ? undefined : t("rail.noMore")}
            onDropdownVisibleChange={(open) => { if (!open) setAdding(false); }}
            onSelect={(hca: string) => edit({ op: "add", hca, node_uuid: node.node_uuid })}
          />
        ) : (
          <Button size="small" type="dashed" icon={<PlusOutlined />} disabled={busy} onClick={() => { load(); setAdding(true); }}>{t("rail.add")}</Button>
        )}
        <SourceTag source={node.hca_source} busy={busy} onReset={() => edit({ op: "reset", node_uuid: node.node_uuid })} />
      </Space>
    </Space>
  );
}

/** One rail: its name, its address ("—" for InfiniBand without IPoIB) and why it is unusable. */
export function RailLine({ rail, children }: { rail: NetworkRail; children?: React.ReactNode }) {
  const { t } = useTranslation("networks");
  const gap = railGapText(t, rail.gap);
  return (
    <Space size={6}>
      <Typography.Text code>{rail.name}</Typography.Text>
      <Typography.Text type="secondary">{rail.ip || "—"}</Typography.Text>
      {gap && (
        <Tooltip title={gap}>
          <ExclamationCircleFilled role="img" aria-label={gap} tabIndex={0} style={{ color: colors.warning }} />
        </Tooltip>
      )}
      {children}
    </Space>
  );
}

/**
 * An RDMA network's bootstrap address on one node, and a way to pin it to one
 * of the node's interfaces or let it be inferred again. Pinning a node reached
 * through a label creates a node attachment for it.
 */
export function BootstrapCell({ network, node, onChanged }: { network: NetworkDetail; node: NetworkNode; onChanged: (detail: NetworkDetail) => void }) {
  const { t } = useTranslation("networks");
  const { inventory, loading, load } = useInventory(node.node_uuid);
  const [editing, setEditing] = useState(false);
  const [busy, setBusy] = useState(false);
  const attachment = network.attachments.find((a) => a.target === "node" && a.node_uuid === node.node_uuid);
  const save = async (iface: string) => {
    setBusy(true);
    try {
      const detail = attachment
        ? await api.updateNetworkAttachment(network.id, attachment.id, { interface: iface, ip: "", hcas: attachment.hcas })
        : await api.attachNetwork(network.id, { node_uuid: node.node_uuid, interface: iface });
      setEditing(false);
      onChanged(detail);
    } catch (e) {
      message.error(t("message.failed", { detail: (e as Error).message }));
    } finally {
      setBusy(false);
    }
  };
  const options = (inventory?.interfaces ?? [])
    .filter((i) => i.addrs.length > 0)
    .map((i) => ({ value: i.name, label: [i.name, ...i.addrs].join(" · ") }));
  return (
    <Space direction="vertical" size={2}>
      {node.ip ? <Typography.Text code>{[node.interface, node.ip].filter(Boolean).join(" · ")}</Typography.Text> : <Typography.Text type="danger">{t("rail.noBootstrap")}</Typography.Text>}
      <Space size={4} wrap>
        {editing ? (
          <Select
            autoFocus
            size="small"
            style={{ minWidth: 240 }}
            open
            loading={loading}
            placeholder={t("rail.pickInterface")}
            options={options}
            onDropdownVisibleChange={(open) => { if (!open) setEditing(false); }}
            onSelect={(iface: string) => save(iface)}
          />
        ) : (
          <Button size="small" type="link" disabled={busy} onClick={() => { load(); setEditing(true); }}>{t("rail.changeBootstrap")}</Button>
        )}
        <SourceTag source={node.bootstrap_source} busy={busy} onReset={attachment ? () => save("") : undefined} />
      </Space>
    </Space>
  );
}

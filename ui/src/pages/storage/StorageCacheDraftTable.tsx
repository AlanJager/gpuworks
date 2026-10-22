import { useMemo, useState } from "react";
import { Button, Input, Select, Space, Tag, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { Storage } from "../../storageTypes";
import type { Node } from "../../api";
import { nodeDisplayName } from "../../nodeDisplay";
import { formatBytes } from "../../format";
import ResourceTable, { type ResourceColumn } from "../../components/resourceTable/ResourceTable";
import StorageCacheDraftEditor from "./StorageCacheDraftEditor";
import { appliesToAllNodes, type NodeOverride, type StorageFormValues } from "./storageForm";

export function draftNodeApplies(values: StorageFormValues, node: Node, members?: ReadonlySet<string>): boolean {
  if (appliesToAllNodes(values) || (members ? members.has(node.uuid) : values.nodes.some((n) => n.node_uuid === node.uuid && !n.override_only))) return true;
  if (values.scope !== "labels" && values.scope !== "mixed") return false;
  return values.labels.some(({ key, value }) => !!key.trim() && (
    Object.prototype.hasOwnProperty.call(node.labels ?? {}, key.trim()) && (!value.trim() || node.labels?.[key.trim()] === value.trim())
  ));
}

export default function StorageCacheDraftTable({ values, nodes, storage, onChange }: {
  values: StorageFormValues;
  nodes: Node[];
  storage?: Storage;
  onChange: (patch: Partial<StorageFormValues>) => void;
}) {
  const { t } = useTranslation("storage");
  const [search, setSearch] = useState("");
  const [policy, setPolicy] = useState("all");
  const [online, setOnline] = useState("all");
  const [selected, setSelected] = useState<string[]>([]);
  const [editing, setEditing] = useState<NodeOverride[] | null>(null);
  const overrides = useMemo(() => new Map(values.nodes.map((n) => [n.node_uuid, n])), [values.nodes]);
  const members = useMemo(() => new Set(values.nodes.filter((n) => !n.override_only).map((n) => n.node_uuid)), [values.nodes]);
  const applies = (node: Node) => draftNodeApplies(values, node, members);
  const all = useMemo(() => {
    const known = new Set(nodes.map((n) => n.uuid));
    const missing = values.nodes.filter((n) => !known.has(n.node_uuid)).map((n) => ({ uuid: n.node_uuid, hostname: n.node_uuid, state: "offline" } as Node));
    return [...nodes, ...missing].filter((node) => draftNodeApplies(values, node, members) || overrides.has(node.uuid));
  }, [nodes, values, overrides, members]);
  const filtered = all.filter((node) => {
    const own = overrides.get(node.uuid)?.cache_max_bytes != null;
    return `${nodeDisplayName(node)} ${node.management_address ?? ""} ${node.uuid}`.toLowerCase().includes(search.toLowerCase()) &&
      (policy === "all" || (policy === "custom" ? own : policy === "inactive" ? !applies(node) : !own)) &&
      (online === "all" || node.state === online);
  });
  const draft = (uuid: string): NodeOverride => ({ ...(overrides.get(uuid) ?? { node_uuid: uuid, path: "", cache_path: "", existing: false, override_only: true }) });
  const apply = (updates: NodeOverride[]) => {
    const changed = new Map(updates.map((n) => [n.node_uuid, n]));
    onChange({ nodes: [...values.nodes.map((n) => changed.get(n.node_uuid) ?? n), ...updates.filter((n) => !overrides.has(n.node_uuid))] });
    setEditing(null);
  };
  const defaultBytes = (values.cacheMaxGiB ?? 0) * 1024 ** 3;
  const limitText = (value: number) => value === 0 ? t("cachePolicy.unlimited") : Number.isFinite(value) ? formatBytes(value) : t("form.error.cacheLimit");
  const columns: ResourceColumn<Node>[] = [
    { key: "node", role: "name", title: t("nodes.column.node"), width: 180, render: (_, node) => <Space direction="vertical" size={0}>{nodeDisplayName(node)}{!applies(node) && <Tag>{t("cacheDraft.inactive")}</Tag>}</Space> },
    { key: "path", title: t("form.cachePath"), width: 220, render: (_, node) => overrides.get(node.uuid)?.cache_path || values.cachePath || t("cacheDraft.defaultDirectory") },
    { key: "capacity", title: t("cacheDraft.available"), width: 150, render: (_, node) => {
      const path = overrides.get(node.uuid)?.cache_path || values.cachePath;
      const usage = node.state === "online" && path ? node.path_usage?.find((item) => item.path === path) : undefined;
      return usage ? formatBytes(usage.available_bytes) : t(node.state === "online" ? "cacheDraft.unverified" : "cacheDraft.offline");
    } },
    { key: "limit", title: t("cachePolicy.limit"), width: 210, render: (_, node) => {
      const value = overrides.get(node.uuid)?.cache_max_bytes;
      return value == null ? t("cacheDraft.defaultLimit", { value: limitText(defaultBytes) }) : limitText(value);
    } },
    { key: "actions", role: "actions", width: 110, render: (_, node) => <Button type="link" onClick={() => setEditing([draft(node.uuid)])}>{t("cacheDraft.configure")}</Button> },
  ];
  return <Space direction="vertical" style={{ width: "100%" }}>
    <Typography.Text type="secondary">{t("cacheDraft.summary", { count: all.length, own: values.nodes.filter((n) => n.cache_max_bytes != null).length })}</Typography.Text>
    <Space wrap>
      <Button disabled={!selected.length} onClick={() => setEditing(selected.map(draft))}>{t("cacheDraft.batch")}</Button>
      <Button disabled={!selected.length} onClick={() => apply(selected.map((uuid) => ({ ...draft(uuid), cache_max_bytes: undefined })))}>{t("cacheDraft.restoreLimit")}</Button>
      <Typography.Text>{t("cacheDraft.selected", { count: selected.length })}</Typography.Text>
      <Button type="link" disabled={!filtered.length} onClick={() => setSelected(filtered.map((n) => n.uuid))}>{t("cacheDraft.selectFiltered", { count: filtered.length })}</Button>
      <Button type="link" disabled={!selected.length} onClick={() => setSelected([])}>{t("cacheDraft.clearSelection")}</Button>
    </Space>
    <ResourceTable<Node> tableId="storage-cache-draft" rowKey="uuid" columns={columns} data={filtered} rowSelection={{ selectedRowKeys: selected, preserveSelectedRowKeys: true, onChange: (keys) => setSelected(keys.map(String)) }} emptyDescription={t("cacheDraft.empty")} toolbar={<Space wrap>
      <Input.Search allowClear aria-label={t("nodes.search")} placeholder={t("nodes.search")} value={search} onChange={(e) => setSearch(e.target.value)} />
      <Select aria-label={t("cacheDraft.policyFilter")} value={policy} onChange={setPolicy} options={["all", "inherit", "custom", "inactive"].map((value) => ({ value, label: t(`cacheDraft.filter.${value}`) }))} />
      <Select aria-label={t("cacheDraft.onlineFilter")} value={online} onChange={setOnline} options={["all", "online", "offline"].map((value) => ({ value, label: t(`cacheDraft.filter.${value}`) }))} />
    </Space>} />
    {editing && <StorageCacheDraftEditor rows={editing} defaultBytes={defaultBytes} defaultPath={values.cachePath} storage={storage} onClose={() => setEditing(null)} onApply={apply} />}
  </Space>;
}

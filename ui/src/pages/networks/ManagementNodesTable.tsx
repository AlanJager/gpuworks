import { useMemo, useState } from "react";
import { detailExpandable } from "../../components/detail/DetailDrawer";
import { Alert, Button, Input, Popconfirm, Select, Space, Switch, Table, Tooltip, Typography, message } from "antd";
import type { ColumnsType } from "antd/es/table";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { useIsAdmin } from "../../components/useIsAdmin";
import { apiErrorText } from "../../apiError";
import type { NetworkNode, NetworkRole } from "../../networkTypes";
import { Endpoint, WarningMark } from "./networkDisplay";

const ROLES: NetworkRole[] = ["serving", "storage_sync", "compute"];
type BulkChoice = "keep" | "allow" | "deny";

/** "key=value" for every label of a node, the way the label filter lists them. */
function labelPairs(labels: Record<string, string> | undefined): string[] {
  return Object.entries(labels ?? {}).map(([k, v]) => `${k}=${v}`);
}

/**
 * Every node on the management network with a switch per role it may carry.
 * Administrators flip one node's role in place, or filter by name, address or
 * label, select nodes and set several roles on all of them at once. Only the
 * roles changed are sent; each node keeps its other settings.
 */
export default function ManagementNodesTable({ nodes, onChanged }: { nodes: NetworkNode[]; onChanged: () => void }) {
  const { t } = useTranslation("networks");
  const { t: tCommon } = useTranslation("common");
  // Until the identity is known, or if the lookup fails, the table is read-only.
  const isAdmin = useIsAdmin();
  const [search, setSearch] = useState("");
  const [labelFilter, setLabelFilter] = useState<string[]>([]);
  const [selected, setSelected] = useState<string[]>([]);
  const [bulk, setBulk] = useState<Record<NetworkRole, BulkChoice>>({ serving: "keep", storage_sync: "keep", compute: "keep" });
  const [busy, setBusy] = useState<string | null>(null);

  const labelOptions = useMemo(() => {
    const all = new Set<string>();
    // Each row carries its node's labels; no node list is fetched for them.
    for (const n of nodes) for (const pair of labelPairs(n.labels)) all.add(pair);
    return [...all].sort().map((pair) => ({ value: pair, label: pair }));
  }, [nodes]);

  const visible = useMemo(() => {
    const q = search.trim().toLowerCase();
    return nodes.filter((n) => {
      if (q && !n.node_name.toLowerCase().includes(q) && !n.ip.includes(q) && !n.interface.toLowerCase().includes(q)) return false;
      const pairs = labelPairs(n.labels);
      return labelFilter.every((pair) => pairs.includes(pair));
    });
  }, [nodes, search, labelFilter]);

  const apply = async (uuids: string[], allow: NetworkRole[], deny: NetworkRole[], key: string) => {
    setBusy(key);
    try {
      const { updated } = await api.updateManagementRoles(uuids, allow, deny);
      message.success(t("management.bulk.done", { count: updated }));
      onChanged();
      return true;
    } catch (e) {
      message.error(apiErrorText(tCommon, e));
      return false;
    } finally {
      setBusy(null);
    }
  };

  // The header says what the switch is about; hovering it says what turning it off does.
  const roleColumn = (role: NetworkRole) => ({
    title: (
      <Tooltip title={t(`management.roleTip.${role}`)}>
        <span className="header-help">{t(`management.roleColumn.${role}`)}</span>
      </Tooltip>
    ),
    key: role,
    render: (_: unknown, n: NetworkNode) => {
      const allowed = !(n.roles_off ?? []).includes(role);
      const key = `${n.node_uuid}/${role}`;
      return (
        <Space size={6}>
          <Switch
            size="small"
            aria-label={`${n.node_name} ${t(`management.roleSwitch.${role}`)}`}
            checked={allowed}
            disabled={!isAdmin || (busy !== null && busy !== key)}
            loading={busy === key}
            onChange={(on) => void apply([n.node_uuid], on ? [role] : [], on ? [] : [role], key)}
          />
          {allowed && !n.roles.includes(role) && <Typography.Text type="secondary">{t("management.dedicated")}</Typography.Text>}
        </Space>
      );
    },
  });

  const columns: ColumnsType<NetworkNode> = [
    {
      title: t("nodeTable.node"),
      dataIndex: "node_name",
      render: (_, n) => <span><Link to="/nodes" state={{ nodeUUID: n.node_uuid }} className="detail-name-cap" title={n.node_name} onClick={(event) => event.stopPropagation()}>{n.node_name}</Link><WarningMark warnings={n.warnings} /></span>,
    },
    { title: t("management.address"), key: "endpoint", render: (_, n) => <Endpoint iface={n.interface} ip={n.ip} /> },
    { title: t("management.carried"), key: "carried", children: ROLES.map(roleColumn) },
  ];
  // A node's labels sit one step down: the row keeps the node, its address
  // and the role switches, which is what fits the 720px detail drawer.
  const labelsOf = (n: NetworkNode) => (
    <dl className="detail-extra">
      <div style={{ gridColumn: "1 / -1" }}>
        <dt>{t("management.labels")}</dt>
        <dd>{labelPairs(n.labels).join(", ") || "—"}</dd>
      </div>
    </dl>
  );

  const allow = ROLES.filter((r) => bulk[r] === "allow");
  const deny = ROLES.filter((r) => bulk[r] === "deny");
  const choiceOptions = (["keep", "allow", "deny"] as const).map((c) => ({ value: c, label: t(`management.bulk.${c}`) }));

  return (
    <Space direction="vertical" size={12} style={{ width: "100%" }}>
      <Alert type="info" showIcon message={t("management.switchIntro")} description={t("management.switchEffect")} />
      <Space wrap>
        <Input.Search allowClear placeholder={t("management.search")} value={search} onChange={(e) => setSearch(e.target.value)} style={{ width: 240 }} aria-label={t("management.search")} />
        <Select
          mode="multiple"
          allowClear
          placeholder={t("management.labelFilter")}
          aria-label={t("management.labelFilter")}
          options={labelOptions}
          value={labelFilter}
          onChange={setLabelFilter}
          style={{ minWidth: 220 }}
        />
        <Typography.Text type="secondary">{t("management.shown", { shown: visible.length, total: nodes.length })}</Typography.Text>
      </Space>
      {isAdmin && (
        <Space wrap>
          <Typography.Text strong>{t("management.bulk.label")}</Typography.Text>
          <Button size="small" onClick={() => setSelected(visible.map((n) => n.node_uuid))} disabled={visible.length === 0}>
            {t("management.bulk.selectShown", { count: visible.length })}
          </Button>
          <Typography.Text>{t("management.bulk.selected", { count: selected.length })}</Typography.Text>
          {ROLES.map((role) => (
            <Space key={role} size={4}>
              <Typography.Text type="secondary">{t(`management.roleColumn.${role}`)}</Typography.Text>
              <Select
                size="small"
                aria-label={`${t("management.bulk.title")} ${t(`role.${role}`)}`}
                value={bulk[role]}
                options={choiceOptions}
                onChange={(v) => setBulk((b) => ({ ...b, [role]: v }))}
                style={{ width: 110 }}
              />
            </Space>
          ))}
          <Popconfirm
            title={t("management.bulk.confirm", { count: selected.length })}
            onConfirm={async () => {
              if (await apply(selected, allow, deny, "bulk")) {
                setSelected([]);
                setBulk({ serving: "keep", storage_sync: "keep", compute: "keep" });
              }
            }}
            okText={t("management.bulk.apply")}
            cancelText={t("actions.cancel")}
            disabled={selected.length === 0 || (allow.length === 0 && deny.length === 0)}
          >
            <Button type="primary" size="small" loading={busy === "bulk"} disabled={selected.length === 0 || (allow.length === 0 && deny.length === 0)}>
              {t("management.bulk.apply")}
            </Button>
          </Popconfirm>
        </Space>
      )}
      <Table<NetworkNode>
        rowKey="node_uuid"
        className="detail-expandable"
        size="small"
        // The management network holds every node of the fleet.
        pagination={{ defaultPageSize: 20, hideOnSinglePage: true, showSizeChanger: true, pageSizeOptions: [10, 20, 50, 100] }}
        columns={columns}
        dataSource={visible}
        rowSelection={isAdmin ? { selectedRowKeys: selected, onChange: (keys) => setSelected(keys as string[]), preserveSelectedRowKeys: true } : undefined}
        locale={{ emptyText: nodes.length === 0 ? t("detail.noNodes") : t("management.noMatch") }}
        scroll={{ x: "max-content" }}
        expandable={detailExpandable<NetworkNode>(labelsOf, { expand: t("detail.expand"), collapse: t("detail.collapse") })}
      />
    </Space>
  );
}

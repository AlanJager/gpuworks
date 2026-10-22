import { useEffect, useState } from "react";
import { Alert, Button, Popconfirm, Space, Table, Tag, Typography, message } from "antd";
import type { ColumnsType } from "antd/es/table";
import { ArrowDownOutlined, ArrowUpOutlined, PlusOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { api, usePolling, type PullProxy } from "../../../api";
import PullProxyDrawer from "./PullProxyDrawer";
import PullProxyPreview from "./PullProxyPreview";
import { moved, pullErrorText, requirementChips } from "./pullFormat";

/**
 * PullProxies lists the proxy entries in the order they are matched: a node
 * uses the first entry whose node requirements it meets, at most one, and
 * none means its agent keeps its own environment. Credentials are never
 * shown, only whether a password is stored.
 */
export default function PullProxies() {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const query = usePolling(() => api.pullProxies(), 0, []);
  const [proxies, setProxies] = useState<PullProxy[]>([]);
  const [editing, setEditing] = useState<PullProxy | "new" | null>(null);
  const [ordering, setOrdering] = useState(false);
  const [actionError, setActionError] = useState("");

  useEffect(() => {
    if (query.data) setProxies(query.data.items);
  }, [query.data]);

  const reorder = async (index: number, delta: number) => {
    const next = moved(proxies, index, delta);
    if (next === proxies) return;
    setOrdering(true);
    setActionError("");
    try {
      const result = await api.orderPullProxies(next.map((proxy) => proxy.id));
      setProxies(result.items);
    } catch (err) {
      setActionError(pullErrorText(t, tCommon, err));
    } finally {
      setOrdering(false);
    }
  };

  const remove = async (proxy: PullProxy) => {
    setActionError("");
    try {
      await api.deletePullProxy(proxy.id);
      message.success(t("pull.proxies.deleted"));
      await query.refetch();
    } catch (err) {
      setActionError(pullErrorText(t, tCommon, err));
    }
  };

  const dash = <Typography.Text type="secondary">{t("pull.proxies.none")}</Typography.Text>;
  const address = (value: string) => (value ? <Typography.Text code>{value}</Typography.Text> : dash);

  const columns: ColumnsType<PullProxy> = [
    {
      title: t("pull.proxies.order"),
      key: "order",
      width: 120,
      render: (_: unknown, _proxy, index) => (
        <Space size={4}>
          <span>{index + 1}</span>
          <Button
            size="small"
            type="text"
            aria-label={t("pull.proxies.moveUp", { n: index + 1 })}
            icon={<ArrowUpOutlined aria-hidden />}
            disabled={ordering || index === 0}
            onClick={() => void reorder(index, -1)}
          />
          <Button
            size="small"
            type="text"
            aria-label={t("pull.proxies.moveDown", { n: index + 1 })}
            icon={<ArrowDownOutlined aria-hidden />}
            disabled={ordering || index === proxies.length - 1}
            onClick={() => void reorder(index, 1)}
          />
        </Space>
      ),
    },
    { title: t("pull.proxies.httpProxy"), dataIndex: "http_proxy", render: address },
    { title: t("pull.proxies.httpsProxy"), dataIndex: "https_proxy", render: address },
    { title: t("pull.proxies.noProxy"), dataIndex: "no_proxy", render: (value: string) => (value ? <Typography.Text>{value}</Typography.Text> : dash) },
    {
      title: t("pull.proxies.user"),
      key: "user",
      render: (_: unknown, proxy) =>
        proxy.username || proxy.has_password ? (
          <Space direction="vertical" size={0}>
            {proxy.username && <Typography.Text>{proxy.username}</Typography.Text>}
            {proxy.has_password && <Tag color="blue">{t("pull.proxies.passwordSet")}</Tag>}
          </Space>
        ) : dash,
    },
    {
      title: t("pull.proxies.controlPlane"),
      dataIndex: "control_plane",
      render: (on: boolean) => (on ? <Tag color="geekblue">{t("pull.proxies.controlPlane")}</Tag> : dash),
    },
    {
      title: t("pull.proxies.appliesTo"),
      key: "nodes",
      render: (_: unknown, proxy) => {
        const chips = requirementChips(proxy.node_requirements);
        return (
          <Space direction="vertical" size={4}>
            <Space size={[4, 4]} wrap>
              {chips.length === 0 ? <Tag>{t("pull.proxies.allNodes")}</Tag> : chips.map((chip) => <Tag key={chip}>{chip}</Tag>)}
            </Space>
            <Typography.Text type="secondary">{t("pull.proxies.matchedNodes", { count: proxy.matched_nodes })}</Typography.Text>
          </Space>
        );
      },
    },
    { title: t("pull.proxies.note"), dataIndex: "note", render: (note: string) => (note ? <Typography.Text type="secondary">{note}</Typography.Text> : dash) },
    {
      title: t("pull.proxies.actions"),
      key: "actions",
      fixed: "right",
      width: 140,
      render: (_: unknown, proxy, index) => (
        <Space size={4}>
          <Button size="small" type="link" onClick={() => setEditing(proxy)}>{t("pull.action.edit")}</Button>
          <Popconfirm
            title={t("pull.proxies.deleteConfirm")}
            description={t("pull.proxies.deleteConfirmBody")}
            okText={t("pull.action.delete")}
            cancelText={t("pull.action.cancel")}
            okButtonProps={{ danger: true }}
            onConfirm={() => remove(proxy)}
          >
            <Button size="small" type="link" danger aria-label={t("pull.proxies.deleteProxy", { n: index + 1 })}>{t("pull.action.delete")}</Button>
          </Popconfirm>
        </Space>
      ),
    },
  ];

  return (
    <Space direction="vertical" size={12} style={{ width: "100%" }}>
      <Typography.Paragraph type="secondary" style={{ marginBottom: 0 }}>{t("pull.proxies.intro")}</Typography.Paragraph>
      <PullProxyPreview refreshKey={proxies} />
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 12, flexWrap: "wrap" }}>
        <Typography.Text type="secondary">{t("pull.proxies.orderHint")}</Typography.Text>
        <Button type="primary" icon={<PlusOutlined aria-hidden />} onClick={() => setEditing("new")}>{t("pull.proxies.new")}</Button>
      </div>
      {query.error && (
        <Alert
          type="error"
          showIcon
          message={t("pull.loadFailed", { detail: query.error.message })}
          action={<Button size="small" onClick={() => void query.refetch()}>{t("pull.action.retry")}</Button>}
        />
      )}
      {actionError && <Alert type="error" showIcon role="alert" message={actionError} closable onClose={() => setActionError("")} />}
      <div role="region" aria-label={t("pull.proxies.tableLabel")}>
        <Table<PullProxy>
          rowKey="id"
          size="small"
          pagination={false}
          loading={query.loading && !query.data}
          columns={columns}
          dataSource={proxies}
          scroll={{ x: "max-content" }}
          locale={{ emptyText: query.error ? " " : t("pull.proxies.empty") }}
        />
      </div>
      {editing && (
        <PullProxyDrawer
          proxy={editing === "new" ? null : editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            message.success(t("pull.proxies.saved"));
            void query.refetch();
          }}
        />
      )}
    </Space>
  );
}

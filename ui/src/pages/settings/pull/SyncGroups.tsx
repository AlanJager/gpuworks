import { useState } from "react";
import { Alert, Button, Popconfirm, Space, Table, Tag, Typography, message } from "antd";
import type { ColumnsType } from "antd/es/table";
import { PlusOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { api, usePolling, type SyncGroup } from "../../../api";
import SyncGroupDrawer from "./SyncGroupDrawer";
import { kibToMiB, pullErrorText, requirementChips } from "./pullFormat";

/**
 * SyncGroups lists the groups of nodes that copy images and models from one
 * another. A node in no group never copies automatically, a node in several
 * may copy from any of them, and the strictest rate of the groups two nodes
 * share applies. An installation starts with one group selecting every node
 * (default-all-nodes), an ordinary group like any other.
 */
export default function SyncGroups() {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const groupsQuery = usePolling(() => api.syncGroups(), 0, []);
  const [editing, setEditing] = useState<SyncGroup | "new" | null>(null);
  const [actionError, setActionError] = useState("");

  const remove = async (group: SyncGroup) => {
    setActionError("");
    try {
      await api.deleteSyncGroup(group.id);
      message.success(t("pull.groups.deleted"));
      await groupsQuery.refetch();
    } catch (err) {
      setActionError(pullErrorText(t, tCommon, err));
    }
  };

  const columns: ColumnsType<SyncGroup> = [
    {
      title: t("pull.groups.name"),
      key: "name",
      render: (_: unknown, group) => (
        <Space size={8}>
          <Typography.Text strong>{group.name}</Typography.Text>
          <Typography.Text type="secondary">#{group.id}</Typography.Text>
        </Space>
      ),
    },
    {
      title: t("pull.groups.selector"),
      key: "selector",
      render: (_: unknown, group) => {
        const chips = requirementChips(group.node_requirements);
        return (
          <Space size={[4, 4]} wrap>
            {chips.length === 0 ? <Tag>{t("pull.rules.allNodes")}</Tag> : chips.map((chip) => <Tag key={chip}>{chip}</Tag>)}
          </Space>
        );
      },
    },
    {
      title: t("pull.groups.matched"),
      dataIndex: "matched_nodes",
      width: 120,
    },
    {
      title: t("pull.groups.rateLimit"),
      dataIndex: "rate_limit_kib_s",
      render: (kib: number) => (kib > 0 ? t("pull.groups.rateShown", { rate: kibToMiB(kib) }) : t("pull.groups.unlimited")),
    },
    {
      title: t("pull.rules.actions"),
      key: "actions",
      fixed: "right",
      width: 140,
      render: (_: unknown, group) => (
        <Space size={4}>
          <Button size="small" type="link" onClick={() => setEditing(group)}>{t("pull.action.edit")}</Button>
          <Popconfirm
            title={t("pull.groups.deleteConfirm", { name: group.name, id: group.id })}
            description={t("pull.groups.deleteConfirmBody")}
            okText={t("pull.action.delete")}
            cancelText={t("pull.action.cancel")}
            okButtonProps={{ danger: true }}
            onConfirm={() => remove(group)}
          >
            <Button size="small" type="link" danger aria-label={t("pull.groups.deleteGroup", { name: group.name, id: group.id })}>{t("pull.action.delete")}</Button>
          </Popconfirm>
        </Space>
      ),
    },
  ];

  return (
    <Space direction="vertical" size={12} style={{ width: "100%" }}>
      <Typography.Paragraph type="secondary" style={{ marginBottom: 0 }}>{t("pull.groups.intro")}</Typography.Paragraph>
      <Alert type="info" showIcon message={t("pull.groups.rules")} />
      <div style={{ display: "flex", justifyContent: "flex-end" }}>
        <Button type="primary" icon={<PlusOutlined aria-hidden />} onClick={() => setEditing("new")}>{t("pull.groups.new")}</Button>
      </div>
      {groupsQuery.error && (
        <Alert
          type="error"
          showIcon
          message={t("pull.loadFailed", { detail: groupsQuery.error.message })}
          action={<Button size="small" onClick={() => void groupsQuery.refetch()}>{t("pull.action.retry")}</Button>}
        />
      )}
      {actionError && <Alert type="error" showIcon role="alert" message={actionError} closable onClose={() => setActionError("")} />}
      <div role="region" aria-label={t("pull.groups.tableLabel")}>
        <Table<SyncGroup>
          rowKey="id"
          size="small"
          pagination={false}
          loading={groupsQuery.loading && !groupsQuery.data}
          columns={columns}
          dataSource={groupsQuery.data?.items ?? []}
          scroll={{ x: "max-content" }}
          locale={{ emptyText: groupsQuery.error ? " " : t("pull.groups.empty") }}
        />
      </div>
      {editing && (
        <SyncGroupDrawer
          group={editing === "new" ? null : editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            message.success(t("pull.groups.saved"));
            void groupsQuery.refetch();
          }}
        />
      )}
    </Space>
  );
}

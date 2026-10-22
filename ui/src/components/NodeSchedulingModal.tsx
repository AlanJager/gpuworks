import { Alert, Button, Modal, Popconfirm, Space, Table, Tag, Tooltip, Typography, message } from "antd";
import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { api, type Node, type NodeReplica, type NodeReplicas, type NodeScheduling } from "../api";
import { apiErrorText } from "../apiError";
import { nodeDisplayName } from "../nodeDisplay";
import { runtimeGpuBlockedReason } from "../nodes/runtimeGpuBlock";

const DRAIN_REFRESH_MS = 5000;

const schedulingColor: Record<NodeScheduling, string | undefined> = {
  schedulable: undefined,
  cordoned: "orange",
  draining: "volcano",
};

const kindColor: Record<NodeReplica["kind"], string> = {
  pool: "blue",
  pinned: "gold",
  named: "default",
};

// NodeSchedulingTag marks a node that takes no new replicas; a schedulable
// node shows nothing. A cordon the platform applied because the runtime
// cannot give containers their accelerators says so on hover.
export function NodeSchedulingTag({ node }: { node: Node }) {
  const { t } = useTranslation("nodes");
  const scheduling = node.scheduling ?? "schedulable";
  if (scheduling === "schedulable") return null;
  const tag = <Tag color={schedulingColor[scheduling]}>{t(`scheduling.state.${scheduling}`)}</Tag>;
  const reason = scheduling === "cordoned" ? runtimeGpuBlockedReason(t, node) : "";
  if (!reason) return tag;
  return <Tooltip title={reason}>{tag}</Tooltip>;
}

// NodeSchedulingModal cordons, drains or uncordons a node and follows a
// drain to empty: what still runs there and whether the drain moves it. A
// replica a drain cannot move (pinned to the node, a multi-node or PD rank)
// is evicted only on the operator's word.
export function NodeSchedulingModal({ node, onClose, onChanged }: { node: Node; onClose: () => void; onChanged: (node: Node) => void }) {
  const { t } = useTranslation("nodes");
  const { t: tCommon } = useTranslation("common");
  const [scheduling, setScheduling] = useState<NodeScheduling>(node.scheduling ?? "schedulable");
  const [replicas, setReplicas] = useState<NodeReplicas | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const next = await api.nodeReplicas(node.uuid);
      setReplicas(next);
      setScheduling(next.scheduling);
      setError("");
    } catch (err) {
      setError(apiErrorText(tCommon, err));
    } finally {
      setLoading(false);
    }
  }, [node.uuid, tCommon]);

  useEffect(() => {
    void load();
  }, [load]);

  // A drain moves replicas over minutes; follow it while the modal is open.
  useEffect(() => {
    if (scheduling !== "draining") return;
    const timer = window.setInterval(() => void load(), DRAIN_REFRESH_MS);
    return () => window.clearInterval(timer);
  }, [scheduling, load]);

  const change = async (next: NodeScheduling) => {
    setBusy(true);
    setError("");
    try {
      const updated = await api.setNodeScheduling(node.uuid, next);
      setScheduling(updated.scheduling ?? next);
      message.success(t(`scheduling.done.${next}`));
      onChanged(updated);
      await load();
    } catch (err) {
      setError(apiErrorText(tCommon, err));
    } finally {
      setBusy(false);
    }
  };

  const evict = async () => {
    setBusy(true);
    setError("");
    try {
      const result = await api.evictNode(node.uuid);
      message.success(t("scheduling.evicted", { count: result.evicted.length }));
      await load();
    } catch (err) {
      setError(apiErrorText(tCommon, err));
    } finally {
      setBusy(false);
    }
  };

  const items = replicas?.items ?? [];
  const stuck = items.filter((item) => item.kind !== "pool").length;

  return (
    <Modal open title={t("scheduling.title", { node: nodeDisplayName(node) })} onCancel={onClose} footer={null} width={760}>
      <Space direction="vertical" style={{ width: "100%" }}>
        <Space wrap>
          <Typography.Text>{t("scheduling.current")}</Typography.Text>
          <Tag color={schedulingColor[scheduling]}>{t(`scheduling.state.${scheduling}`)}</Tag>
        </Space>
        <Typography.Paragraph type="secondary" style={{ margin: 0 }}>{t(`scheduling.help.${scheduling}`)}</Typography.Paragraph>
        <Space wrap>
          {scheduling !== "cordoned" && <Button loading={busy} onClick={() => void change("cordoned")}>{t("scheduling.action.cordon")}</Button>}
          {scheduling !== "draining" && <Button loading={busy} onClick={() => void change("draining")}>{t("scheduling.action.drain")}</Button>}
          {scheduling !== "schedulable" && (
            // The platform cordons a node whose runtime cannot give a
            // container its accelerators, and will do so again on the next
            // report. The operator may know better, so this warns rather
            // than refuses.
            node.runtime_gpu_blocked ? (
              <Popconfirm
                title={t("scheduling.blocked.title")}
                description={<span style={{ whiteSpace: "pre-line", maxWidth: 420, display: "inline-block" }}>{t("scheduling.blocked.description", { reason: runtimeGpuBlockedReason(t, node) })}</span>}
                okText={t("scheduling.action.uncordonAnyway")}
                cancelText={t("action.cancel")}
                onConfirm={() => void change("schedulable")}
              >
                <Button type="primary" loading={busy}>{t("scheduling.action.uncordon")}</Button>
              </Popconfirm>
            ) : (
              <Button type="primary" loading={busy} onClick={() => void change("schedulable")}>{t("scheduling.action.uncordon")}</Button>
            )
          )}
          {scheduling !== "schedulable" && items.length > 0 && (
            <Popconfirm title={t("scheduling.action.evictConfirm")} onConfirm={() => void evict()} okText={t("scheduling.action.evict")} cancelText={t("action.cancel")}>
              <Button danger loading={busy}>{t("scheduling.action.evict")}</Button>
            </Popconfirm>
          )}
        </Space>
        {error && <Alert type="error" showIcon message={error} />}
        {scheduling === "draining" && stuck > 0 && <Alert type="warning" showIcon message={t("scheduling.stuck", { count: stuck })} />}
        <Typography.Text strong>{t("scheduling.replicas", { count: items.length })}</Typography.Text>
        <Table<NodeReplica>
          rowKey="instance"
          size="small"
          loading={loading}
          dataSource={items}
          pagination={false}
          locale={{ emptyText: t("scheduling.empty") }}
          columns={[
            { title: t("scheduling.columns.deployment"), dataIndex: "deployment" },
            { title: t("scheduling.columns.instance"), dataIndex: "instance", render: (uuid: string) => <Typography.Text code>{uuid.slice(0, 12)}</Typography.Text> },
            { title: t("scheduling.columns.state"), dataIndex: "state", width: 110 },
            {
              title: t("scheduling.columns.drain"),
              dataIndex: "kind",
              render: (kind: NodeReplica["kind"]) => <Tag color={kindColor[kind]}>{t(`scheduling.kind.${kind}`)}</Tag>,
            },
          ]}
        />
      </Space>
    </Modal>
  );
}

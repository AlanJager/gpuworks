import { useEffect, useState } from "react";
import { Alert, Button, Modal, Space, Table, Tag, Typography } from "antd";
import type { ColumnsType } from "antd/es/table";
import { useTranslation } from "react-i18next";
import { api, type KubeCluster, type KubeClusterOperation, type KubeObjectAction, type KubeObjectResult } from "../../api";
import { apiErrorText } from "../../apiError";

const ACTION_COLOR: Record<KubeObjectAction, string> = {
  create: "green",
  update: "blue",
  delete: "red",
  absent: "default",
  kept: "gold",
  failed: "red",
  not_tried: "default",
  dependent: "default",
};

type Stage = "checking" | "checked" | "running" | "done";

/**
 * Applies a cluster entry's manifest to its cloud cluster, or removes it,
 * on the operator's confirmation: it first asks the API server to check
 * every change (a dry run), shows what each object would go through, and
 * only then does it.
 */
export default function KubeClusterOperationModal({ cluster, op, onClose, onDone }: {
  cluster: KubeCluster;
  op: "apply" | "remove";
  onClose: () => void;
  onDone?: () => void;
}) {
  const { t } = useTranslation("kubernetes");
  const { t: tCommon } = useTranslation("common");
  const [stage, setStage] = useState<Stage>("checking");
  const [result, setResult] = useState<KubeClusterOperation | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    let alive = true;
    setStage("checking");
    setResult(null);
    setError(null);
    api.kubeClusterOperation(cluster.uuid, op, true)
      .then((r) => { if (alive) { setResult(r); setStage("checked"); } })
      .catch((e) => { if (alive) { setError(apiErrorText(tCommon, e)); setStage("checked"); } });
    return () => { alive = false; };
  }, [cluster.uuid, op, attempt, tCommon]);

  const run = async () => {
    setStage("running");
    setError(null);
    try {
      const r = await api.kubeClusterOperation(cluster.uuid, op, false);
      setResult(r);
      if (!r.code) onDone?.();
    } catch (e) {
      setError(apiErrorText(tCommon, e));
    } finally {
      setStage("done");
    }
  };

  const failed = Boolean(error || result?.code);
  const source = cluster.source;
  const columns: ColumnsType<KubeObjectResult> = [
    { title: t("operation.column.kind"), dataIndex: "kind", width: 170 },
    {
      title: t("operation.column.name"),
      key: "name",
      render: (_, r) => <Typography.Text code>{r.namespace ? `${r.namespace}/${r.name}` : r.name}</Typography.Text>,
    },
    {
      title: t("operation.column.action"),
      key: "action",
      width: 260,
      render: (_, r) => (
        <Space direction="vertical" size={2}>
          <Tag color={ACTION_COLOR[r.action] ?? "default"}>{t(`operation.${stage === "done" ? "done" : "plan"}.${r.action}`, { defaultValue: r.action })}</Tag>
          {(r.error || r.reason) && <Typography.Text type={r.error ? "danger" : "secondary"} style={{ fontSize: 12 }}>{r.error || r.reason}</Typography.Text>}
        </Space>
      ),
    },
  ];
  const rows = result?.results ?? [];

  const title = t(op === "apply" ? "operation.applyTitle" : "operation.removeTitle", { name: cluster.name });
  const footer = stage === "done" || (stage === "checked" && failed) ? (
    <Space>
      {stage === "checked" && failed && <Button onClick={() => setAttempt((n) => n + 1)}>{t("operation.retry")}</Button>}
      <Button type="primary" onClick={onClose}>{t("operation.close")}</Button>
    </Space>
  ) : (
    <Space>
      <Button onClick={onClose} disabled={stage === "running"}>{t("operation.cancel")}</Button>
      <Button type="primary" danger={op === "remove"} disabled={stage !== "checked"} loading={stage === "running"} onClick={() => void run()}>
        {t(op === "apply" ? "operation.confirmApply" : "operation.confirmRemove")}
      </Button>
    </Space>
  );

  return (
    <Modal open title={title} onCancel={stage === "running" ? undefined : onClose} footer={footer} width={760} destroyOnClose maskClosable={false}>
      <Space direction="vertical" size={12} style={{ width: "100%" }}>
        <Typography.Paragraph style={{ marginBottom: 0 }}>
          {t(op === "apply" ? "operation.applyIntro" : "operation.removeIntro", {
            account: source?.provider_name || source?.provider_kind || "",
            cluster: source?.cluster_id ?? "",
          })}
        </Typography.Paragraph>
        {stage === "checking" && <Alert type="info" showIcon message={t("operation.checking")} />}
        {stage === "checked" && !failed && <Alert type="info" showIcon message={t(op === "apply" ? "operation.checkedApply" : "operation.checkedRemove")} />}
        {stage === "running" && <Alert type="info" showIcon message={t(op === "apply" ? "operation.applying" : "operation.removing")} />}
        {stage === "done" && !failed && (
          <Alert type="success" showIcon message={t(op === "apply" ? "operation.applied" : "operation.removed")} description={op === "apply" ? t("operation.appliedNext") : undefined} />
        )}
        {failed && (
          <Alert
            type="error"
            showIcon
            message={result?.params ? t(`operation.failed.${result.params.step}`) : t("operation.failed.other")}
            description={result?.params?.reason ?? error}
          />
        )}
        {rows.length > 0 && (
          <Table<KubeObjectResult>
            rowKey={(r) => `${r.kind}/${r.namespace ?? ""}/${r.name}`}
            size="small"
            pagination={false}
            columns={columns}
            dataSource={rows}
            title={() => t(stage === "done" ? "operation.resultTitle" : "operation.planTitle")}
          />
        )}
      </Space>
    </Modal>
  );
}

import { useState } from "react";
import { Button, Popconfirm, Radio, Space, Tag, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type Deployment } from "../api";
import { ConfirmOrderModal, UnitRetryAlert } from "./OrderDialogs";
import { useOrder } from "./useOrder";

// DeploymentLifecycleActions are the drawer header's start, restart and stop
// actions; they apply to every deployment shape. A stopped deployment offers
// only Start. Restart asks how (rolling by default); stop keeps the
// deployment (and the drawer) so it can be started again.
export default function DeploymentLifecycleActions({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const { busy, run } = useOrder();
  const [restartOpen, setRestartOpen] = useState(false);
  const deleting = dep.desired_state === "deleting";

  const order = async (action: "start" | "stop") => {
    const refused = await run(() => (action === "start" ? api.startDeployment(dep.id) : api.stopDeployment(dep.id)));
    if (refused !== null) {
      message.error(t(action === "start" ? "detail.startFailed" : "detail.stopFailed", { detail: refused }));
      return;
    }
    message.success(t(action === "start" ? "detail.started" : "detail.stopped"));
    onChanged();
  };

  if (dep.desired_state === "stopped") {
    return (
      <Button type="primary" loading={busy} onClick={() => void order("start")}>
        {t("detail.start")}
      </Button>
    );
  }
  return (
    <>
      {dep.restart_requested_at && <Tag color="processing">{t("detail.restart.rolling")}</Tag>}
      <Button disabled={deleting} onClick={() => setRestartOpen(true)}>
        {t("detail.restart.action")}
      </Button>
      <Popconfirm title={t("detail.stopConfirm")} onConfirm={() => void order("stop")} okText={t("detail.stop")} cancelText={t("detail.cancel")}>
        <Button danger loading={busy} disabled={deleting}>
          {t("detail.stop")}
        </Button>
      </Popconfirm>
      <RestartConfirm open={restartOpen} deploymentID={dep.id} onClose={() => setRestartOpen(false)} onChanged={onChanged} />
    </>
  );
}

// RestartConfirm asks how to restart: rolling (the default, one replica at a
// time while the rest serve) or all at once (unavailable until every
// instance is ready again).
function RestartConfirm({ open, deploymentID, onClose, onChanged }: { open: boolean; deploymentID: number; onClose: () => void; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const [strategy, setStrategy] = useState<"rolling" | "all">("rolling");
  return (
    <ConfirmOrderModal
      open={open}
      title={t("detail.restart.confirmTitle")}
      okText={t("detail.restart.confirmOk")}
      danger={strategy === "all"}
      failKey="detail.restart.failed"
      order={() => api.restartDeployment(deploymentID, strategy)}
      onClose={onClose}
      onDone={() => {
        message.success(t("detail.restart.started"));
        onChanged();
      }}
    >
      <Radio.Group value={strategy} onChange={(e) => setStrategy(e.target.value)}>
        <Space direction="vertical">
          <Radio value="rolling">{t("detail.restart.strategyRolling")}</Radio>
          <Radio value="all">{t("detail.restart.strategyAll")}</Radio>
        </Space>
      </Radio.Group>
      <Typography.Paragraph type="secondary" style={{ marginTop: 12 }}>
        {t(strategy === "rolling" ? "detail.restart.rollingDescription" : "detail.restart.confirmDescription")}
      </Typography.Paragraph>
    </ConfirmOrderModal>
  );
}

// ReplicaRetryAlert is an ordinary deployment's give-up notice: the replica
// the controller stopped rebuilding, its newest failure, and its Retry (a PD
// deployment shows this per unit instead).
export function ReplicaRetryAlert({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const exhausted = dep.retry_exhausted;
  if (!exhausted || dep.pd) return null;
  return (
    <UnitRetryAlert
      message={t("detail.retry.exhausted", { attempts: exhausted.failed_attempts, reason: exhausted.last_failure ?? "" })}
      retry={() => api.retryDeploymentUnit(dep.id, { role: "", index: exhausted.index })}
      onDone={onChanged}
    />
  );
}

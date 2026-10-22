import { useState, type ReactNode } from "react";
import { Alert, Button, Modal } from "antd";
import { useTranslation } from "react-i18next";
import { useOrder } from "./useOrder";

// ConfirmOrderModal asks before an order that takes something down (a
// restart of the deployment or of one replica). It stays open while the order
// is on its way and shows the server's refusal inside it; on success it
// closes and reports done.
export function ConfirmOrderModal({ open, title, okText, danger, failKey, order, onClose, onDone, children }: {
  open: boolean;
  title: string;
  okText: string;
  danger?: boolean;
  // failKey is the deployments message for a refusal, with {{detail}}.
  failKey: string;
  order: () => Promise<unknown>;
  onClose: () => void;
  onDone: () => void;
  children: ReactNode;
}) {
  const { t } = useTranslation("deployments");
  const { busy, run } = useOrder();
  const [failure, setFailure] = useState<string | null>(null);
  const confirm = async () => {
    setFailure(null);
    const refused = await run(order);
    if (refused !== null) {
      setFailure(t(failKey, { detail: refused }));
      return;
    }
    onClose();
    onDone();
  };
  return (
    <Modal
      title={title}
      open={open}
      onCancel={() => {
        if (busy) return;
        // A refusal belongs to the attempt it answered, not the next open.
        setFailure(null);
        onClose();
      }}
      onOk={() => void confirm()}
      okText={okText}
      cancelText={t("detail.cancel")}
      okButtonProps={{ danger, loading: busy }}
      cancelButtonProps={{ disabled: busy }}
      maskClosable={!busy}
      closable={!busy}
    >
      {children}
      {failure && <Alert style={{ marginTop: 12 }} type="error" showIcon message={failure} />}
    </Modal>
  );
}

// UnitRetryAlert is a replica's give-up notice: what gave up and why, and a
// Retry that gives it its budget back without touching the other replicas
// (ADR 0029). A refused retry is shown on the notice itself.
export function UnitRetryAlert({ message, retry, onDone }: { message: string; retry: () => Promise<unknown>; onDone: () => void }) {
  const { t } = useTranslation("deployments");
  const { busy, run } = useOrder();
  const [failure, setFailure] = useState<string | null>(null);
  const onRetry = async () => {
    setFailure(null);
    const refused = await run(retry);
    if (refused !== null) setFailure(refused);
    else onDone();
  };
  return (
    <Alert
      style={{ marginTop: 12 }}
      type="error"
      showIcon
      message={message}
      description={failure ? t("detail.retry.failed", { detail: failure }) : undefined}
      action={
        <Button size="small" loading={busy} onClick={() => void onRetry()}>
          {t("detail.retry.action")}
        </Button>
      }
    />
  );
}

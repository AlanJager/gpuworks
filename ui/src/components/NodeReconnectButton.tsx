import { useState } from "react";
import { Button, Modal, Tooltip, Typography, message } from "antd";
import { useTranslation } from "react-i18next";

import { api, type Node } from "../api";
import { nodeConnection } from "../nodes/connectionState";
import { useRecoveryProbe } from "../nodes/recoveryProbe";
import { useCopy } from "./CopyButton";

/**
 * Bringing a node's agent back, as one action wherever its connection is
 * shown. The platform does it over SSH when it has a credential for the
 * node; without one the only way in is the node's own install command, so
 * the button hands that over instead of being disabled with no way forward.
 */
export function NodeReconnectButton({ node, onUpdated, size = "small" }: {
  node: Node;
  onUpdated: (node: Node) => void;
  size?: "small" | "middle";
}) {
  const { t } = useTranslation("nodes");
  const { loading, probe } = useRecoveryProbe(node, onUpdated);
  const [command, setCommand] = useState<string | null>(null);
  const [copy, copyHolder] = useCopy();

  // Already connected, or being connected right now: nothing to bring
  // back. Read the same way the badge reads it, so the button and the
  // state beside it never disagree.
  if (nodeConnection(node).state !== "offline") return null;

  // The instance behind the node is gone: SSH has nothing to reach and the
  // install command nothing to run on, so there is no way back to offer.
  const gone = node.recovery_phase === "stopped";

  const byHand = async () => {
    try {
      const full = await api.node(node.uuid);
      onUpdated(full);
      setCommand(full.install_command ?? "");
    } catch (error) {
      message.error(t("recovery.message.probeFailed", { detail: (error as Error).message }));
    }
  };

  return (
    <>
      {copyHolder}
      <Tooltip title={gone ? t("recovery.reconnectGone") : undefined}>
        {/* A disabled button swallows hover; the span carries it. */}
        <span style={{ display: "inline-block" }}>
          <Button
            size={size}
            loading={loading}
            disabled={gone}
            onClick={node.has_ssh_credential ? probe : byHand}
          >
            {t("recovery.reconnect")}
          </Button>
        </span>
      </Tooltip>
      <Modal
        open={command !== null}
        title={t("recovery.manual.title")}
        onCancel={() => setCommand(null)}
        onOk={() => { if (command) void copy(command, { success: t("recovery.manual.copied") }); }}
        okText={t("recovery.manual.copy")}
        okButtonProps={{ disabled: !command }}
      >
        <Typography.Paragraph type="secondary">{t("recovery.manual.hint")}</Typography.Paragraph>
        {command
          ? <pre style={{ whiteSpace: "pre-wrap", wordBreak: "break-all", margin: 0 }}>{command}</pre>
          : <Typography.Text type="warning">{t("recovery.manual.unavailable")}</Typography.Text>}
      </Modal>
    </>
  );
}

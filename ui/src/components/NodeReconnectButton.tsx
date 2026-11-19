import { useState } from "react";
import { useTranslation } from "react-i18next";

import { api, type Node } from "../api";
import { nodeConnection } from "../nodes/connectionState";
import { useRecoveryProbe } from "../nodes/recoveryProbe";
import { useCopy } from "./CopyButton";
import { Button } from "./ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "./ui/dialog";
import { Tooltip } from "./ui/tooltip";
import { message } from "../ui-kit/feedback";

const DIALOG_STYLE = { width: "min(520px, calc(100vw - 32px))", maxWidth: "calc(100vw - 32px)" };

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
  const { t: tCommon } = useTranslation("common");
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
      {(() => {
        const button = (
          // A disabled button swallows hover; the span carries it.
          <span className="inline-block">
            <Button
              variant="tertiary"
              size={size === "small" ? "sm" : "md"}
              loading={loading}
              disabled={gone}
              onClick={node.has_ssh_credential ? probe : byHand}
            >
              {t("recovery.reconnect")}
            </Button>
          </span>
        );
        return gone ? <Tooltip content={t("recovery.reconnectGone")}>{button}</Tooltip> : button;
      })()}
      <Dialog open={command !== null} onOpenChange={(open) => { if (!open) setCommand(null); }}>
        <DialogContent closeButtonLabel={tCommon("actions.close")} style={DIALOG_STYLE}>
          <DialogHeader><DialogTitle>{t("recovery.manual.title")}</DialogTitle></DialogHeader>
          <p className="mb-3 text-fg-muted">{t("recovery.manual.hint")}</p>
          {command
            ? <pre className="m-0 whitespace-pre-wrap break-all">{command}</pre>
            : <p className="text-fg-warning">{t("recovery.manual.unavailable")}</p>}
          <DialogFooter>
            <Button variant="ghost" onClick={() => setCommand(null)}>{tCommon("actions.cancel")}</Button>
            <Button variant="primary" disabled={!command} onClick={() => { if (command) void copy(command, { success: t("recovery.manual.copied") }); }}>
              {t("recovery.manual.copy")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}

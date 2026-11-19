import { Badge } from "./ui/badge";
import { Tooltip } from "./ui/tooltip";
import { ExclamationCircleFilled } from "@ant-design/icons";
import { useTranslation } from "react-i18next";

import type { Node } from "../api";
import { nodeConnection } from "../nodes/connectionState";
import { recoveryErrorText } from "../nodes/recoveryError";
import { detailBadgeTone } from "../pages/state";
import { formatTime } from "../i18n/datetime";

type ConnectionNode = Pick<Node, "state" | "recovery_phase" | "last_recovery_error" | "last_recovery_code" | "last_seen_at" | "last_probe_at" | "recovery_attempts">;

/**
 * A node's connection, shown the same way wherever it appears. Recovery is
 * a warning mark on the state rather than a status beside it: an operator
 * reads "connecting" and hovers only if they care why.
 */
export function NodeConnectionBadge({ node }: { node: ConnectionNode }) {
  const { t } = useTranslation("nodes");
  const connection = nodeConnection(node);
  const note = connection.noteKey ? t(connection.noteKey) : "";
  const detail = [note, recoveryErrorText(t, node.last_recovery_code, node.last_recovery_error)].filter(Boolean).join("\n");
  return (
    <Tooltip content={<ConnectionHistory node={node} detail={detail} />}>
      <span tabIndex={0} className="inline-flex items-center gap-1" aria-label={t(connection.labelKey) + " · " + (detail || t("connectionDiagnostics.historyTitle"))}>
        <Badge variant="plain" size="lg" status={detailBadgeTone[connection.badge]}>{t(connection.labelKey)}</Badge>
        {detail && (
          <ExclamationCircleFilled
            role="img"
            aria-label={detail}
            className="cursor-help text-fg-warning"
          />
        )}
      </span>
    </Tooltip>
  );
}

// Tooltip mounts its content on demand; unopened rows do not format dates or
// build history DOM, and the data already arrived with the node list.
function ConnectionHistory({ node, detail }: { node: ConnectionNode; detail: string }) {
  const { t } = useTranslation("nodes");
  const unknown = t("connectionDiagnostics.unknown");
  return (
    <div>
      {detail && <div style={{ whiteSpace: "pre-line" }}>{detail}</div>}
      <div>{t("connectionDiagnostics.lastSeen")}: {formatTime(node.last_seen_at, unknown)}</div>
      <div>{t("connectionDiagnostics.lastRecovery")}: {formatTime(node.last_probe_at, unknown)}</div>
      <div>{t("connectionDiagnostics.attempts", { count: node.recovery_attempts ?? 0 })}</div>
    </div>
  );
}

import { Badge, Space, Tooltip } from "antd";
import { ExclamationCircleFilled } from "@ant-design/icons";
import { useTranslation } from "react-i18next";

import type { Node } from "../api";
import { nodeConnection } from "../nodes/connectionState";
import { colors } from "../theme/tokens";

/**
 * A node's connection, shown the same way wherever it appears. Recovery is
 * a warning mark on the state rather than a status beside it: an operator
 * reads "connecting" and hovers only if they care why.
 */
export function NodeConnectionBadge({ node }: { node: Pick<Node, "state" | "recovery_phase" | "last_recovery_error"> }) {
  const { t } = useTranslation("nodes");
  const connection = nodeConnection(node);
  const note = connection.noteKey ? t(connection.noteKey) : "";
  const detail = [note, node.last_recovery_error].filter(Boolean).join("\n");
  return (
    <Space size={4}>
      <Badge status={connection.badge} text={t(connection.labelKey)} />
      {detail && (
        <Tooltip trigger={["hover", "focus"]} title={<span style={{ whiteSpace: "pre-line" }}>{detail}</span>}>
          <ExclamationCircleFilled
            role="img"
            aria-label={detail}
            tabIndex={0}
            style={{ color: colors.warning, cursor: "help" }}
          />
        </Tooltip>
      )}
    </Space>
  );
}

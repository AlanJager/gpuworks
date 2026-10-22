import { Progress, Space, Tooltip, Typography } from "antd";
import { ExclamationCircleFilled } from "@ant-design/icons";
import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";
import type { Storage, StorageNodeStatus } from "../../storageTypes";
import { storageReasonText, storageStateLabel } from "../../storageDisplay";
import { nodeRowName } from "../../nodeDisplay";

const previewLimit = 5;

/** "node-47: failed · nfs client missing" */
export function problemLine(t: TFunction, status: StorageNodeStatus): string {
  const reason = status.reason ? storageReasonText(t, status.reason) : "";
  const detail = [reason, status.message].filter(Boolean).join(" · ");
  const head = `${nodeRowName(status)}: ${storageStateLabel(t, status.state)}`;
  return detail ? `${head} · ${detail}` : head;
}

/**
 * A storage's problem nodes split in two: nodes that are offline say nothing
 * about the storage, nodes that are online but not ready do.
 */
export function splitProblems(problems: StorageNodeStatus[]): { offline: StorageNodeStatus[]; failing: StorageNodeStatus[] } {
  return {
    offline: problems.filter((p) => p.state === "offline"),
    failing: problems.filter((p) => p.state !== "offline"),
  };
}

/** The first few problem nodes of a storage as lines of text, with a count of the rest. */
export function problemLines(t: TFunction, problems: StorageNodeStatus[]): string[] {
  const lines = problems.slice(0, previewLimit).map((status) => problemLine(t, status));
  if (problems.length > previewLimit) lines.push(t("health.more", { ns: "storage", count: problems.length - previewLimit }));
  return lines;
}

/**
 * How many of the nodes a storage applies to have it ready, a small bar, and a
 * warning mark whose tooltip names the nodes that do not.
 */
export default function StorageNodeHealth({ storage }: { storage: Storage }) {
  const { t } = useTranslation("storage");
  const { total, ready, problems } = storage.nodes;
  if (total === 0) return <Typography.Text type="secondary">{t("health.none")}</Typography.Text>;
  const { offline, failing } = splitProblems(problems);
  const online = total - offline.length;
  const text = problemLines(t, failing).join("\n");
  return (
    <Space size={8} align="center" wrap>
      <Typography.Text>{online > 0 ? t("health.summary", { ready, total: online }) : t("health.allOffline")}</Typography.Text>
      {online > 0 && <Progress percent={Math.round((ready / online) * 100)} size="small" showInfo={false} status={ready === online ? "success" : "normal"} style={{ width: 64, margin: 0 }} />}
      {failing.length > 0 && (
        <Tooltip trigger={["hover", "focus"]} title={<span style={{ whiteSpace: "pre-line" }}>{text}</span>}>
          <ExclamationCircleFilled role="img" aria-label={t("health.problems", { count: failing.length })} tabIndex={0} style={{ color: "#faad14", cursor: "help" }} />
        </Tooltip>
      )}
      {offline.length > 0 && <Typography.Text type="secondary">{t("health.offline", { count: offline.length })}</Typography.Text>}
    </Space>
  );
}

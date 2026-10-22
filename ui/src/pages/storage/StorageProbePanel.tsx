import { Alert, Descriptions, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { StorageProbeResult } from "../../storageTypes";
import { formatBytes } from "../../format";
import { nodeRowName } from "../../nodeDisplay";

/** What a trial mount found: version, capacity, readable and writable, or why it failed and what the command printed. */
export default function StorageProbePanel({ result }: { result: StorageProbeResult }) {
  const { t } = useTranslation("storage");
  const yes = t("probe.yes");
  const no = t("probe.no");
  if (!result.ok) {
    return (
      <Alert
        type="error"
        showIcon
        message={t("probe.failed", { node: nodeRowName(result) })}
        description={
          <div>
            {(result.message || result.reason) && <div>{result.message || result.reason}</div>}
            {result.output && <Typography.Paragraph code style={{ whiteSpace: "pre-wrap", marginTop: 8, marginBottom: 0 }}>{result.output}</Typography.Paragraph>}
          </div>
        }
      />
    );
  }
  return (
    <Alert
      type={result.writable ? "success" : "warning"}
      showIcon
      message={t("probe.ok", { node: nodeRowName(result) })}
      description={
        <div>
          <Descriptions size="small" column={2}>
            {result.version && <Descriptions.Item label={t("probe.version")}>{result.version}</Descriptions.Item>}
            {result.options && <Descriptions.Item label={t("probe.options")}>{result.options}</Descriptions.Item>}
            <Descriptions.Item label={t("probe.readable")}>{result.readable ? yes : no}</Descriptions.Item>
            <Descriptions.Item label={t("probe.writable")}>{result.writable ? yes : no}</Descriptions.Item>
            {result.total_bytes ? <Descriptions.Item label={t("probe.capacity")}>{t("probe.capacityValue", { available: formatBytes(result.available_bytes ?? 0), total: formatBytes(result.total_bytes) })}</Descriptions.Item> : null}
          </Descriptions>
          {!result.writable && <Typography.Text type="warning">{t("probe.readOnlyNotice")}</Typography.Text>}
        </div>
      }
    />
  );
}

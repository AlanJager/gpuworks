import { Space, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { RuntimeMount } from "../../api";
import CopyButton from "../../components/CopyButton";

export function mountClipboardText(mounts: RuntimeMount[]): string {
  if (!mounts.length) return "";
  return [
    "# Mounts (runtime configuration)",
    ...mounts.map((mount) => `# ${JSON.stringify(mount)}`),
  ].join("\n");
}

export default function DeployMountPreview({
  mounts,
  copyLabel,
}: {
  mounts: RuntimeMount[];
  copyLabel?: string;
}) {
  const { t } = useTranslation("deploy");
  if (!mounts.length) return null;
  return (
    <div className="deploy-mount-preview">
      <div className="deploy-mount-preview-heading">
        <Typography.Text strong>{t("runtimeCommand.mounts")}</Typography.Text>
        {copyLabel && (
          <CopyButton
            size="small"
            label={copyLabel}
            text={() => mountClipboardText(mounts)}
          />
        )}
      </div>
      <div className="deploy-mount-list">
        {mounts.map((mount, index) => (
          <div className="deploy-mount-row" key={`${mount.target}:${index}`}>
            <Space size={6} wrap>
              <Typography.Text type="secondary">
                {mount.source != null
                  ? t("runtimeCommand.source")
                  : t("runtimeCommand.volume")}
              </Typography.Text>
              <Typography.Text code>
                {mount.source ?? mount.volume ?? ""}
              </Typography.Text>
              <span aria-hidden="true">→</span>
              <Typography.Text type="secondary">
                {t("runtimeCommand.target")}
              </Typography.Text>
              <Typography.Text code>{mount.target}</Typography.Text>
              <Typography.Text>
                {t(
                  mount.read_only
                    ? "runtimeCommand.readOnly"
                    : "runtimeCommand.readWrite",
                )}
              </Typography.Text>
              <Typography.Text type="secondary">
                {t("runtimeCommand.purpose")}
              </Typography.Text>
              <Typography.Text code>{mount.purpose ?? ""}</Typography.Text>
            </Space>
          </div>
        ))}
      </div>
    </div>
  );
}

import { Space, Tag, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { formatTime } from "../i18n/datetime";
import type { ServerBuildInfo } from "../serverBuildInfo";

// A candidate update must never lend its release label to another running build.
export default function ServerBuildSummary({ version, build }: { version: string; build?: ServerBuildInfo }) {
  const { t } = useTranslation("common");
  const info = build?.version === version ? build : undefined;
  const kind = info && ["stable", "prerelease", "development"].includes(info.kind) ? info.kind : "unknown";
  const colors = { stable: "success", prerelease: "warning", development: "orange", unknown: "default" };
  return <Space direction="vertical" size={4} style={{ width: "100%" }}>
    <Space wrap><Tag color={colors[kind]}>{t(`buildInfo.kind.${kind}`)}</Tag><Typography.Text strong>{version}</Typography.Text></Space>
    <Typography.Text>{t(`buildInfo.description.${kind}`)}</Typography.Text>
    {info?.built_at && <Typography.Text type="secondary">{t("buildInfo.builtAt", { time: formatTime(info.built_at) })}</Typography.Text>}
    {info?.published_version ? <Typography.Text type="secondary">{t("buildInfo.matched", { version: info.published_version })}{info.published_at && ` · ${t("buildInfo.publishedAt", { time: formatTime(info.published_at) })}`}</Typography.Text>
      : <Typography.Text type="secondary">{t("buildInfo.unmatched")}</Typography.Text>}
    {(info?.commit || info?.build_channel) && <details>
      <summary>{t("buildInfo.details")}</summary>
      <Space direction="vertical" size={2}>
        {info.commit && <Typography.Text code>{t("buildInfo.commit", { commit: info.commit })}</Typography.Text>}
        {info.build_channel && <Typography.Text>{t("buildInfo.channel", { channel: info.build_channel })}</Typography.Text>}
      </Space>
    </details>}
  </Space>;
}

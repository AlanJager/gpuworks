import { Space, Switch, Tag, Tooltip, Typography } from "antd";
import type { ColumnsType } from "antd/es/table";
import type { ImageCatalogEntry } from "../../api";
import { bytes, rollingTag, upstreamGoneTag } from "./ImagePageHelpers";
import type { TFunction } from "i18next";

const { Text } = Typography;

type Props = {
  seriesEnabled: boolean;
  onSelect: (row: ImageCatalogEntry) => void;
  onToggle: (row: ImageCatalogEntry, enabled: boolean) => void;
};

// Catalog metadata is read-only here: a system image changes only through a
// manifest, and removing a community or custom version is an action of the
// detail drawer, not of every row.
export function buildVersionColumns({
  seriesEnabled,
  onSelect,
  onToggle,
}: Props, t: TFunction): ColumnsType<ImageCatalogEntry> {
  return [
    {
      title: t("versionTable.column.tag"),
      dataIndex: "image_ref",
      render: (value: string, row) => (
        <a
          href="#image-version"
          onClick={(event) => {
            event.preventDefault();
            event.stopPropagation();
            onSelect(row);
          }}
        >
          <Text code>{value}</Text>
        </a>
      ),
    },
    { title: t("versionTable.column.engineVersion"), dataIndex: "engine_version" },
    {
      title: t("versionTable.column.frameworkVersion"),
      key: "framework",
      render: (_value, row) => `${row.framework || "—"} ${row.framework_version || ""}`,
    },
    {
      title: t("versionTable.column.labels"),
      key: "tag",
      render: (_value, row) => <>{rollingTag(t, row)}{upstreamGoneTag(t, row)}</>,
    },
    {
      title: (
        <Tooltip title={t("versionTable.sizeHelp")}>
          {t("versionTable.column.size")}
        </Tooltip>
      ),
      dataIndex: "size_bytes",
      render: (value: number) => bytes(value),
    },
    { title: t("versionTable.column.arch"), dataIndex: "arch", render: (value: string) => value || "multi" },
    {
      title: t("versionTable.column.distributed"),
      key: "coverage",
      render: (_value, row) =>
        `${row.present_nodes ?? 0} / ${row.total_nodes ?? 0}`,
    },
    {
      title: t("versionTable.column.status"),
      key: "enabled",
      render: (_value, row) =>
        row.deprecated ? (
          <Tooltip
            title={row.deprecated_since ? t("versionTable.deprecatedSince", { version: row.deprecated_since }) : undefined}
          >
            <Tag color="orange">{t("versionTable.deprecatedRefs", { refs: row.reference_count ?? 0 })}</Tag>
          </Tooltip>
        ) : (
          <Space size={6}>
            <Switch
              size="small"
              aria-label={t("versionTable.enabledLabel", { ref: row.image_ref })}
              checked={row.effective_enabled ?? row.enabled}
              disabled={!seriesEnabled}
              title={!seriesEnabled ? t("versionTable.disabledBySeries") : undefined}
              onClick={(_, event) => event.stopPropagation()}
              onChange={(checked) => onToggle(row, checked)}
            />
            {!seriesEnabled && <Tag>{t("versionTable.disabledBySeries")}</Tag>}
          </Space>
        ),
    },
  ];
}

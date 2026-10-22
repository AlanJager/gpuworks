import { Button, Popconfirm, Space, Switch, Tag, Tooltip, Typography } from "antd";
import { DeleteOutlined, ProfileOutlined } from "@ant-design/icons";
import type { ImageSeries } from "../../api";
import VendorLogo from "../../components/VendorLogo";
import ResourceTable, { type ResourceColumn } from "../../components/resourceTable/ResourceTable";
import { useTranslation } from "react-i18next";

const { Text } = Typography;

type Props = {
  rows: ImageSeries[];
  loading: boolean;
  updatedAt?: Date | null;
  onRefresh?: () => void;
  onOpen: (row: ImageSeries) => void;
  onToggle: (row: ImageSeries, enabled: boolean) => void;
  onDelete: (row: ImageSeries) => void;
};

const distinct = (values: Array<string | undefined>) => [...new Set(values.filter((value): value is string => !!value))].sort();

export default function ImageSeriesTable({ rows, loading, updatedAt, onRefresh, onOpen, onToggle, onDelete }: Props) {
  const { t } = useTranslation("images");
  const columns: ResourceColumn<ImageSeries>[] = [
    {
      key: "name",
      role: "name",
      title: t("seriesTable.column.series"),
      width: 240,
      sortValue: (row) => row.name,
      render: (_value, row) => <Button type="link" onClick={() => onOpen(row)}>{row.name}</Button>,
    },
    {
      key: "vendor",
      title: t("seriesTable.column.vendor"),
      width: 120,
      render: (_value, row) => <VendorLogo framework={row.framework} />,
    },
    {
      key: "engine",
      title: t("seriesTable.column.engine"),
      width: 130,
      sortValue: (row) => row.engine,
      filters: distinct(rows.map((row) => row.engine)).map((value) => ({ value, text: value })),
      onFilter: (value, row) => row.engine === value,
      render: (_value, row) => row.engine ? <Tag color="blue">{row.engine}</Tag> : <Text type="secondary">-</Text>,
    },
    {
      key: "framework",
      title: t("seriesTable.column.framework"),
      width: 150,
      sortValue: (row) => row.framework,
      filters: distinct(rows.map((row) => row.framework)).map((value) => ({ value, text: value })),
      onFilter: (value, row) => row.framework === value,
      render: (_value, row) => row.framework || "—",
    },
    { key: "version_count", title: t("seriesTable.column.count"), width: 100, align: "right", sortValue: (row) => row.version_count, render: (_value, row) => row.version_count },
    {
      key: "coverage",
      title: t("seriesTable.column.coverage"),
      width: 120,
      align: "right",
      sortValue: (row) => row.present_nodes,
      render: (_value, row) => `${row.present_nodes} / ${row.total_nodes}`,
    },
    {
      key: "enabled",
      title: t("seriesTable.column.status"),
      width: 110,
      filters: [{ value: "enabled", text: t("common.enabled") }, { value: "disabled", text: t("common.disabled") }],
      onFilter: (value, row) => (value === "enabled") === row.enabled,
      render: (_value, row) => (
        <Switch
          size="small"
          aria-label={t("seriesTable.enabledLabel", { name: row.name })}
          checked={row.enabled}
          checkedChildren={t("common.enabled")}
          unCheckedChildren={t("common.disabled")}
          onClick={(_, event) => event.stopPropagation()}
          onChange={(checked) => onToggle(row, checked)}
        />
      ),
    },
    {
      key: "actions",
      role: "actions",
      width: 100,
      render: (_value, row) => (
        <Space size={2}>
          <Tooltip title={t("seriesTable.details")}>
            <Button type="text" size="small" icon={<ProfileOutlined />} aria-label={t("seriesTable.details")} onClick={() => onOpen(row)} />
          </Tooltip>
          {/* Official series come from the catalog and are not deleted here. */}
          {row.source !== "official" && (
            <Popconfirm
              title={t("seriesTable.deleteTitle", { name: row.name })}
              description={t("seriesTable.deleteDescription")}
              okText={t("common.delete")}
              cancelText={t("common.cancel")}
              okButtonProps={{ danger: true }}
              onConfirm={() => onDelete(row)}
            >
              <Tooltip title={t("common.delete")}>
                <Button type="text" size="small" danger icon={<DeleteOutlined />} aria-label={t("common.delete")} />
              </Tooltip>
            </Popconfirm>
          )}
        </Space>
      ),
    },
  ];

  return (
    <ResourceTable<ImageSeries>
      tableId="image-series"
      rowKey="id"
      loading={loading}
      data={rows}
      columns={columns}
      updatedAt={updatedAt}
      onRefresh={onRefresh}
    />
  );
}

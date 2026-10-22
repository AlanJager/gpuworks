import {
  Alert,
  Button,
  Card,
  Drawer,
  Popconfirm,
  Space,
  Table,
  Tag,
  Typography,
} from "antd";
import { DeleteOutlined } from "@ant-design/icons";
import { Link } from "react-router-dom";
import type { ColumnsType } from "antd/es/table";
import type {
  ImageCatalogEntry,
  ImageInspection,
  ImageSeries,
  ImageNode,
} from "../../api";
import ImageNodeTable from "./ImageNodeTable";
import ImageBuiltInConfig from "./ImageBuiltInConfig";
import { digestMismatchNodes } from "./ImagePageHelpers";
import { useTranslation } from "react-i18next";

const { Text } = Typography;
type Props = {
  detail: ImageSeries | null;
  onClose: () => void;
  /**
   * The series' versions. Until GET /images/{series_id} answers these are the
   * catalog list's rows, which carry counts but no node rows.
   */
  detailVersions: ImageCatalogEntry[];
  /** Set while the series detail, the only source of node rows, is being read. */
  nodesLoading: boolean;
  versionColumns: ColumnsType<ImageCatalogEntry>;
  selectedVersion: ImageCatalogEntry | null;
  setSelectedVersion: (version: ImageCatalogEntry) => void;
  // The last node inspection of the selected version, if any: read on
  // request and shown as the image's own configuration, never stored.
  inspection: { nodeUUID: string; result: ImageInspection } | null;
  nodeName: (nodeUUID: string) => string;
  inspecting: string | null;
  availablePeer: (version: ImageCatalogEntry, target: ImageNode) => boolean;
  modeFor: (
    version: ImageCatalogEntry,
    target: ImageNode,
  ) => string;
  setMode: (
    version: ImageCatalogEntry,
    nodeUUID: string,
    mode: string,
  ) => void;
  pullVersion: (
    version: ImageCatalogEntry,
    nodeUUID: string,
    retry?: boolean,
  ) => void;
  probeImage: (version: ImageCatalogEntry, nodeUUID: string, sourceNode: string) => Promise<unknown>;
  syncFromPeer: (
    version: ImageCatalogEntry,
    nodeUUID: string,
    sourceNode: string,
    rateLimitKiB: number,
  ) => Promise<void>;
  pullFromURL: (
    version: ImageCatalogEntry,
    nodeUUID: string,
    url: string,
    relay: boolean,
  ) => Promise<void>;
  inspectVersion: (version: ImageCatalogEntry, nodeUUID: string) => void;
  cancelTransfer: (version: ImageCatalogEntry, nodeUUID: string) => void;
  removeFromNode: (version: ImageCatalogEntry, nodeUUID: string) => void;
  repullVersion: (version: ImageCatalogEntry, nodeUUID: string) => void;
  /** Removes a community or custom series; system series have no delete. */
  deleteSeries: (series: ImageSeries) => void;
  /** Removes a community or custom version; system versions have no delete. */
  deleteVersion: (version: ImageCatalogEntry) => void;
};

export default function ImageDetailDrawer({
  detail,
  onClose,
  detailVersions,
  nodesLoading,
  versionColumns,
  selectedVersion,
  setSelectedVersion,
  inspection,
  nodeName,
  inspecting,
  availablePeer,
  modeFor,
  setMode,
  pullVersion,
  probeImage,
  syncFromPeer,
  pullFromURL,
  inspectVersion,
  cancelTransfer,
  removeFromNode,
  repullVersion,
  deleteSeries,
  deleteVersion,
}: Props) {
  const { t } = useTranslation("images");
  // System rows are maintained by the catalog manifest: no delete here.
  const editable = Boolean(detail && detail.source !== "official");
  const mismatched = detailVersions
    .map((version) => ({ version, nodes: digestMismatchNodes(version) }))
    .filter((item) => item.nodes > 0);
  return (
    <Drawer
      title={detail ? t("detail.title", { name: detail.name }) : t("detail.fallbackTitle")}
      open={!!detail}
      onClose={onClose}
      width={1120}
    >
      {detail && (
        <Space direction="vertical" size={16} style={{ width: "100%" }}>
          <Space wrap style={{ width: "100%", justifyContent: "space-between" }}>
            <Space>
              <Tag color="blue">{detail.engine || "-"}</Tag>
              {detail.framework && <Tag>{detail.framework}</Tag>}
            </Space>
            {editable && (
              <Popconfirm
                title={t("seriesTable.deleteTitle", { name: detail.name })}
                description={t("seriesTable.deleteDescription")}
                okText={t("common.delete")}
                cancelText={t("common.cancel")}
                okButtonProps={{ danger: true }}
                onConfirm={() => deleteSeries(detail)}
              >
                <Button danger icon={<DeleteOutlined />}>{t("detail.deleteSeries")}</Button>
              </Popconfirm>
            )}
          </Space>
          {mismatched.map(({ version, nodes }) => (
            <Alert
              key={version.id}
              type="warning"
              showIcon
              message={t("detail.digestMismatch", { ref: version.image_ref, nodes })}
            />
          ))}
          <Table
            rowKey="id"
            size="small"
            dataSource={detailVersions}
            columns={versionColumns}
            pagination={{ defaultPageSize: 8, hideOnSinglePage: true }}
            rowClassName={(row) =>
              row.effective_enabled === false ? "image-version-disabled" : ""
            }
            onRow={(row) => ({ onClick: () => setSelectedVersion(row) })}
          />
          {selectedVersion && (
            <ImageBuiltInConfig version={selectedVersion} inspection={inspection} nodeName={nodeName} />
          )}
          <Card
            size="small"
            title={
              selectedVersion
                ? t("detail.nodeDistributionFor", { ref: selectedVersion.image_ref })
                : t("detail.nodeDistribution")
            }
            extra={
              editable && selectedVersion ? (
                <Popconfirm
                  title={t("versionTable.deleteTitle", { ref: selectedVersion.image_ref })}
                  description={t("versionTable.deleteDescription")}
                  okText={t("common.delete")}
                  cancelText={t("common.cancel")}
                  okButtonProps={{ danger: true }}
                  onConfirm={() => deleteVersion(selectedVersion)}
                >
                  <Button danger size="small" icon={<DeleteOutlined />}>{t("detail.deleteVersion")}</Button>
                </Popconfirm>
              ) : null
            }
          >
            {selectedVersion ? (
              <>
                <ImageNodeTable
                  selectedVersion={selectedVersion}
                  loading={nodesLoading}
                  inspecting={inspecting}
                  availablePeer={availablePeer}
                  modeFor={modeFor}
                  setMode={setMode}
                  pullVersion={pullVersion}
                  probeImage={probeImage}
                  syncFromPeer={syncFromPeer}
                  pullFromURL={pullFromURL}
                  inspectVersion={inspectVersion}
                  cancelTransfer={cancelTransfer}
                  removeFromNode={removeFromNode}
                  repullVersion={repullVersion}
                />
                <Text type="secondary" style={{ display: "block", marginTop: 8 }}>
                  {t("nodeTable.autoNote")}{" "}
                  <Link to="/settings">{t("nodeTable.autoNoteLink")}</Link>
                </Text>
              </>
            ) : (
              <Text type="secondary">
                {t("detail.selectVersion")}
              </Text>
            )}
          </Card>
        </Space>
      )}
    </Drawer>
  );
}

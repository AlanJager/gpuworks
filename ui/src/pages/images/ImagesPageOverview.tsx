import {
  Alert,
  Button,
  Card,
  Col,
  Row,
  Space,
  Statistic,
  Tabs,
  Typography,
  Upload,
} from "antd";
import { PlusOutlined, ReloadOutlined, UploadOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import type { ImageSeries } from "../../api";
import PageHeader from "../../components/PageHeader";
import { bytes } from "./ImagePageHelpers";
import ImageSeriesTable from "./ImageSeriesTable";

const { Text } = Typography;

export type ImageSourceTab = "official" | "community" | "custom";

type Props = {
  sourceTab: ImageSourceTab;
  onSourceTabChange: (tab: ImageSourceTab) => void;
  series: ImageSeries[];
  loading: boolean;
  error?: Error | null;
  updatedAt?: Date | null;
  /** The catalog version the last check or upload in this session brought in. */
  catalogVersion: string;
  syncingCatalog: boolean;
  onSyncCatalog: () => void;
  uploadingManifest: boolean;
  onUploadManifest: (file: File) => void;
  onOpenImport: () => void;
  totalVersions: number;
  distributed: number;
  catalogBytes?: number;
  localBytes?: number;
  catalogUnknown: number;
  localUnknown: number;
  sourceError: string | null;
  onDismissSourceError: () => void;
  onRefresh: () => void;
  onOpenSeries: (row: ImageSeries) => void;
  onToggleSeries: (row: ImageSeries, enabled: boolean) => void;
  onDeleteSeries: (row: ImageSeries) => void;
};

export default function ImagesPageOverview({
  sourceTab,
  onSourceTabChange,
  series,
  loading,
  error,
  updatedAt,
  catalogVersion,
  syncingCatalog,
  onSyncCatalog,
  uploadingManifest,
  onUploadManifest,
  onOpenImport,
  totalVersions,
  distributed,
  catalogBytes,
  localBytes,
  catalogUnknown,
  localUnknown,
  sourceError,
  onDismissSourceError,
  onRefresh,
  onOpenSeries,
  onToggleSeries,
  onDeleteSeries,
}: Props) {
  const { t } = useTranslation("images");
  return (
    <>
      <PageHeader
        title={t("page.title")}
        description={t("page.description")}
        extra={
          <Button icon={<ReloadOutlined />} loading={loading} onClick={onRefresh}>
            {t("page.refresh")}
          </Button>
        }
      />
      <Tabs
        activeKey={sourceTab}
        onChange={(key) => onSourceTabChange(key as ImageSourceTab)}
        items={(["official", "community", "custom"] as const).map((key) => ({
          key,
          label: t(`page.tab.${key}`),
        }))}
      />
      {sourceTab === "official" && (
        <Card size="small" style={{ marginBottom: 12 }}>
          <Space wrap>
            <Button type="primary" loading={syncingCatalog} disabled={uploadingManifest} onClick={onSyncCatalog}>
              {t("page.catalog.update")}
            </Button>
            <Upload
              accept=".json,application/json"
              showUploadList={false}
              disabled={syncingCatalog || uploadingManifest}
              beforeUpload={(file) => {
                onUploadManifest(file);
                return false;
              }}
            >
              <Button icon={<UploadOutlined />} loading={uploadingManifest} disabled={syncingCatalog}>
                {t("page.catalog.upload")}
              </Button>
            </Upload>
            <Text type="secondary">
              {catalogVersion ? t("page.catalog.version", { version: catalogVersion }) : t("page.catalog.hint")}
            </Text>
          </Space>
        </Card>
      )}
      {sourceTab !== "official" && (
        <Space style={{ marginBottom: 12 }}>
          <Button icon={<PlusOutlined />} type="primary" onClick={onOpenImport}>
            {t(sourceTab === "community" ? "page.addCommunity" : "page.addCustom")}
          </Button>
        </Space>
      )}
      <Row gutter={[12, 12]} className="resource-summary-row">
        {[
          ["series", series.length],
          ["images", totalVersions],
          ["distributed", distributed],
          ["catalogSize", bytes(catalogBytes)],
          ["localSize", bytes(localBytes)],
        ].map(([key, value], index) => (
          <Col key={key} xs={24} sm={12} md={8} lg={index === 4 ? 4 : 5}>
            <Card>
              <Statistic title={t(`page.summary.${key}`)} value={value} />
              {(key === "catalogSize" || key === "localSize") && (
                <Text type="secondary">
                  {t(`page.summary.${key}Hint`)}
                  {(key === "catalogSize" ? catalogUnknown : localUnknown) > 0 && (
                    <> · {t("page.summary.unknownSizes", { count: key === "catalogSize" ? catalogUnknown : localUnknown })}</>
                  )}
                </Text>
              )}
            </Card>
          </Col>
        ))}
      </Row>
      {sourceError && (
        <Alert
          type="warning"
          showIcon
          closable
          onClose={onDismissSourceError}
          message={t("page.operationNotice")}
          description={sourceError}
        />
      )}
      {error && (
        <Alert
          type="error"
          showIcon
          message={t("page.loadFailed")}
          description={error.message}
          action={<Button onClick={onRefresh}>{t("common.retry")}</Button>}
        />
      )}
      <Card title={t("page.seriesTitle")}>
        <ImageSeriesTable
          rows={series}
          loading={loading}
          updatedAt={updatedAt}
          onRefresh={onRefresh}
          onOpen={onOpenSeries}
          onToggle={onToggleSeries}
          onDelete={onDeleteSeries}
        />
      </Card>
    </>
  );
}

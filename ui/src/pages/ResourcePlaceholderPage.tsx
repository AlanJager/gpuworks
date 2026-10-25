import { ApiOutlined } from "@ant-design/icons";
import { Alert, Button, Descriptions, Space, Tag, Timeline, Typography } from "antd";
import { useNavigate } from "react-router-dom";
import PageHeader from "../components/PageHeader";
import { useTranslation } from "react-i18next";

type PlaceholderKind = "k8s";

export default function ResourcePlaceholderPage({ kind }: { kind: PlaceholderKind }) {
  const { t } = useTranslation("common");
  const prefix = `resourcePlaceholder.${kind}`;
  const cfg = {
    title: t(`${prefix}.title`),
    description: t(`${prefix}.description`),
    status: t(`${prefix}.status`),
    current: t(`${prefix}.current`),
    next: [0, 1, 2, 3].map((index) => t(`${prefix}.next.${index}`)),
    primary: { to: "/nodes", label: t(`${prefix}.primary`), icon: <ApiOutlined /> },
  };
  const navigate = useNavigate();
  return (
    <>
      <PageHeader
        title={cfg.title}
        description={cfg.description}
        extra={<Tag color="gold">{cfg.status}</Tag>}
      />
      <div style={{ display: "grid", gridTemplateColumns: "minmax(0, 1.2fr) minmax(280px, .8fr)", gap: 16 }}>
        <section className="operational-panel">
          <Space direction="vertical" size={16} style={{ width: "100%" }}>
            <Alert type="info" showIcon message={cfg.current} />
            <Descriptions column={1} size="small" bordered>
              <Descriptions.Item label={t("resourcePlaceholder.deliveryScope")}>{cfg.status}</Descriptions.Item>
              <Descriptions.Item label={t("resourcePlaceholder.currentEntry")}>{cfg.primary.label}</Descriptions.Item>
            </Descriptions>
            <Button type="primary" icon={cfg.primary.icon} onClick={() => navigate(cfg.primary.to, { state: { openAddDrawer: true } })}>
              {cfg.primary.label}
            </Button>
          </Space>
        </section>
        <section className="operational-panel">
          <Typography.Title level={5} style={{ marginTop: 0 }}>
            {t("resourcePlaceholder.futureCapabilities")}
          </Typography.Title>
          <Timeline
            items={cfg.next.map((item) => ({
              children: <Typography.Text>{item}</Typography.Text>,
            }))}
          />
        </section>
      </div>
    </>
  );
}

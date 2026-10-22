import { Button, Result, Space, Typography } from "antd";
import { useLocation, useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";

export default function NotFoundPage() {
  const navigate = useNavigate();
  const location = useLocation();
  const { t } = useTranslation("common");

  return (
    <div style={{ padding: "32px 0" }}>
      <Result
        status="404"
        title={t("notFound.title")}
        subTitle={
          <Space direction="vertical" size={6}>
            <Typography.Text type="secondary">{t("notFound.description")}</Typography.Text>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {location.pathname}
            </Typography.Text>
          </Space>
        }
        extra={
          <Space wrap>
            <Button type="primary" onClick={() => navigate("/dashboard")}>
              {t("notFound.dashboard")}
            </Button>
            <Button onClick={() => navigate("/nodes")}>{t("notFound.nodes")}</Button>
            <Button onClick={() => navigate("/deployments")}>{t("notFound.deployments")}</Button>
          </Space>
        }
      />
    </div>
  );
}

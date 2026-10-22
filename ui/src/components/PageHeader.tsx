import { Breadcrumb, Space, Typography } from "antd";
import {
  ApiOutlined,
  AppstoreOutlined,
  CloudOutlined,
  CloudServerOutlined,
  DashboardOutlined,
  FileSearchOutlined,
  HeartOutlined,
  KeyOutlined,
  RocketOutlined,
  SettingOutlined,
  UnorderedListOutlined,
} from "@ant-design/icons";
import type { ReactNode } from "react";
import { useLocation } from "react-router-dom";

export default function PageHeader({
  title,
  description,
  extra,
  breadcrumb,
  className,
  illustration,
}: {
  title: ReactNode;
  description?: string;
  extra?: ReactNode;
  /** Plain text, or a node such as a link to the parent page. */
  breadcrumb?: ReactNode[];
  className?: string;
  illustration?: string;
}) {
  const location = useLocation();
  return (
    <div className={`page-header${className ? ` ${className}` : ""}`}>
      <div className="page-header-main">
        <span className="page-header-icon" aria-hidden="true">{pageIcon(location.pathname)}</span>
        <div className="page-header-copy">
          {breadcrumb && breadcrumb.length > 0 && (
            <Breadcrumb className="page-header-breadcrumb" items={breadcrumb.map((item) => ({ title: item }))} />
          )}
          <Typography.Title level={2} className="page-header-title">{title}</Typography.Title>
          {description && <Typography.Paragraph className="page-header-description">{description}</Typography.Paragraph>}
        </div>
      </div>
      {illustration && <img className="page-header-illustration" src={illustration} alt="" aria-hidden="true" />}
      {extra && <Space className="page-header-extra">{extra}</Space>}
    </div>
  );
}

function pageIcon(pathname: string) {
  if (pathname.startsWith("/dashboard")) return <DashboardOutlined />;
  if (pathname.startsWith("/monitor")) return <HeartOutlined />;
  if (pathname.startsWith("/nodes")) return <CloudServerOutlined />;
  if (pathname.startsWith("/providers") || pathname.startsWith("/zstack")) return <CloudOutlined />;
  if (pathname.startsWith("/deployments")) return <UnorderedListOutlined />;
  if (pathname.startsWith("/deploy")) return <RocketOutlined />;
  if (pathname.startsWith("/evaluations") || pathname.startsWith("/logs")) return <FileSearchOutlined />;
  if (pathname.startsWith("/apikeys")) return <KeyOutlined />;
  if (pathname.startsWith("/settings")) return <SettingOutlined />;
  if (pathname.startsWith("/networks") || pathname.includes("connect")) return <ApiOutlined />;
  return <AppstoreOutlined />;
}

import { Breadcrumb, Typography } from "antd";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { useLocation } from "react-router-dom";
import { sectionLabelKey } from "../navigation";

/**
 * PageHeader is the title block every page starts with. The breadcrumb names
 * the menu section the page sits in (plus any parents), so it can never
 * disagree with the menu; the description, the actions and a toolbar row are
 * optional. Narrow, the parts stack in reading order instead of drifting.
 */
export default function PageHeader({
  title,
  description,
  extra,
  toolbar,
  parents,
  className,
}: {
  title: ReactNode;
  /** One sentence on what the page is for; kept to a line. */
  description?: string;
  /** Actions beside the title, the primary one last. */
  extra?: ReactNode;
  /** Page-wide controls — filters, a time range, mode tabs — in a row under the title. */
  toolbar?: ReactNode;
  /** Pages between the menu section and this one, e.g. a link back to the list. */
  parents?: ReactNode[];
  className?: string;
}) {
  const { t } = useTranslation("common");
  const location = useLocation();
  // The menu entry decides. Comparing this title to the section text would
  // follow whichever translation happens to match.
  const sectionKey = sectionLabelKey(location.pathname);
  const section = sectionKey ? t(sectionKey) : undefined;
  const trail = [section, ...(parents ?? [])].filter((item) => item !== undefined);
  return (
    <div className={`page-header${className ? ` ${className}` : ""}`}>
      <div className="page-header-row">
        <div className="page-header-copy">
          {trail.length > 0 && (
            <Breadcrumb className="page-header-breadcrumb" items={trail.map((item) => ({ title: item }))} />
          )}
          <Typography.Title level={2} className="page-header-title">
            <span className="page-header-title-text">{title}</span>
          </Typography.Title>
          {description && <Typography.Paragraph className="page-header-description" title={description}>{description}</Typography.Paragraph>}
        </div>
        {extra && <div className="page-header-extra">{extra}</div>}
      </div>
      {toolbar && <div className="page-header-toolbar">{toolbar}</div>}
    </div>
  );
}

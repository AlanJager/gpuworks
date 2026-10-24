import { Breadcrumb, Typography } from "antd";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { useLocation } from "react-router-dom";
import { navGroupForPath } from "../navigation";

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
  breadcrumb,
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
  /**
   * @deprecated The full path as the page used to spell it out; pages move
   * to `parents`, and the section then comes from the menu.
   */
  breadcrumb?: ReactNode[];
  /** @deprecated No longer shown; pages drop it as they move to `parents`. */
  illustration?: string;
  className?: string;
}) {
  const { t } = useTranslation("common");
  const location = useLocation();
  const group = navGroupForPath(location.pathname);
  const section = group ? t(group.labelKey) : undefined;
  // A section named like the page itself (Overview) says nothing new.
  const trail = breadcrumb ?? [section !== title ? section : undefined, ...(parents ?? [])].filter((item) => item !== undefined);
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

import { Typography } from "antd";
import type { ReactNode } from "react";

// DetailSection is one titled block inside a detail tab. Tables inside it
// keep cells on one line and scroll sideways (detailDrawer.css).
export default function DetailSection({ title, extra, children }: { title: string; extra?: ReactNode; children: ReactNode }) {
  return (
    <section className="detail-section">
      <Typography.Title level={5}>
        {title}
        {extra && <span className="detail-section-extra">{extra}</span>}
      </Typography.Title>
      {children}
    </section>
  );
}

import { Typography } from "antd";
import type { ReactNode } from "react";

// NodeDetailSection is one titled block inside a node detail tab.
export default function NodeDetailSection({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="node-detail-section">
      <Typography.Title level={5}>{title}</Typography.Title>
      {children}
    </section>
  );
}

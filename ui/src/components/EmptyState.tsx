import type { ReactNode } from "react";

export type EmptyStateKind = "nodes" | "models" | "deployments" | "data";

const illustrations: Record<EmptyStateKind, string> = {
  nodes: "/assets/empty-nodes.webp",
  models: "/assets/empty-models.webp",
  deployments: "/assets/empty-deployments.webp",
  data: "/assets/empty-data.webp",
};

export default function EmptyState({
  kind = "data",
  description,
  action,
  compact = false,
}: {
  kind?: EmptyStateKind;
  description: ReactNode;
  action?: ReactNode;
  compact?: boolean;
}) {
  return (
    <div className={`empty-state${compact ? " empty-state-compact" : ""}`}>
      <img className="empty-state-image" src={illustrations[kind]} alt="" aria-hidden="true" />
      <div className="empty-state-description">{description}</div>
      {action && <div className="empty-state-action">{action}</div>}
    </div>
  );
}

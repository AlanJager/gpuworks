import { Empty, EmptyMedia, EmptyDescription, EmptyActions } from "./ui/empty";
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
    <Empty reason="no-data" scope={compact ? "inline" : "section"} density={compact ? "compact" : "default"}>
      <EmptyMedia variant="illustration"><img src={illustrations[kind]} alt="" aria-hidden="true" /></EmptyMedia>
      <EmptyDescription>{description}</EmptyDescription>
      {action && <EmptyActions>{action}</EmptyActions>}
    </Empty>
  );
}

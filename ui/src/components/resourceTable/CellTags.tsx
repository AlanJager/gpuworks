import { Badge, type BadgeColor } from "../ui/badge";
import { Tooltip } from "../ui/tooltip";

export interface CellTag {
  key: string;
  text: string;
  color?: BadgeColor;
  /** Shown on hover; the text itself when omitted. */
  tooltip?: string;
}

/**
 * A list cell's tags on one line: the first `limit`, then "+n" whose tooltip
 * lists the rest. A row stays one line high however many values it has.
 * badgeColor colours every tag; otherwise each takes its own colour, grey by default.
 */
export default function CellTags({ tags, limit = 2, empty = "—", badgeColor }: { tags: CellTag[]; badgeColor?: BadgeColor; limit?: number; empty?: string }) {
  if (tags.length === 0) return <>{empty}</>;
  const shown = tags.slice(0, limit);
  const rest = tags.slice(limit);
  return (
    <span className="resource-cell-tags">
      {shown.map((tag) => (
        <Tooltip key={tag.key} content={tag.tooltip ?? tag.text}>
          <Badge size="sm" color={badgeColor ?? tag.color ?? "gray"} className="min-w-0 [&_[data-slot=badge-label]]:truncate">{tag.text}</Badge>
        </Tooltip>
      ))}
      {rest.length > 0 && (
        <Tooltip content={<div className="resource-cell-tags-rest">{rest.map((tag) => <div key={tag.key}>{tag.text}</div>)}</div>}>
          <Badge size="sm" className="shrink-0">+{rest.length}</Badge>
        </Tooltip>
      )}
    </span>
  );
}

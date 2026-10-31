import { useCallback } from "react";
import { Link, useSearchParams } from "react-router-dom";

/**
 * The query param that opens a node's drawer on the scheduling page:
 * `/scheduling?node=<uuid>`, like `/deployments?deployment=<id>` opens a
 * deployment's. Each opening and closing is a history entry, so the
 * browser's back button undoes it.
 */
export const NODE_PARAM = "node";

/** withNode is the page's query with the node param set, or removed for null. */
function withNode(current: URLSearchParams, uuid: string | null): URLSearchParams {
  const next = new URLSearchParams(current);
  if (uuid == null) next.delete(NODE_PARAM);
  else next.set(NODE_PARAM, uuid);
  return next;
}

/**
 * useNodeParam is the node whose drawer the URL opens, and the ways to open
 * and close it; every other query param stays as it is.
 */
export function useNodeParam(): { node: string | null; openNode: (uuid: string) => void; closeNode: () => void } {
  const [searchParams, setSearchParams] = useSearchParams();
  const node = searchParams.get(NODE_PARAM) || null;
  const openNode = useCallback((uuid: string) => setSearchParams((current) => withNode(current, uuid)), [setSearchParams]);
  const closeNode = useCallback(() => setSearchParams((current) => withNode(current, null)), [setSearchParams]);
  return { node, openNode, closeNode };
}

/**
 * NodeLink names a node and opens its drawer on the scheduling page. A node
 * the server no longer knows comes without a UUID and is plain text.
 */
export function NodeLink({ uuid, name }: { uuid?: string; name: string }) {
  const [searchParams] = useSearchParams();
  if (!uuid) return <span>{name}</span>;
  return <Link to={{ search: `?${withNode(searchParams, uuid).toString()}` }}>{name}</Link>;
}

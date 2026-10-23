import { Navigate, useParams, useSearchParams } from "react-router-dom";

// networkDetailPath is where a network's detail opens: its drawer over the
// network list, on a tab if one is named. The management network is 0.
export function networkDetailPath(id: number | string, tab?: string): string {
  return `/networks?network=${id}${tab ? `&tab=${encodeURIComponent(tab)}` : ""}`;
}

// NetworkDetailRedirect keeps the old detail address (/networks/3) working:
// it opens the same network, on the same tab, in the list's drawer.
export function NetworkDetailRedirect() {
  const { id } = useParams();
  const [params] = useSearchParams();
  return <Navigate replace to={networkDetailPath(id ?? "", params.get("tab") ?? undefined)} />;
}

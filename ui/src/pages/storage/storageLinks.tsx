import { Navigate, useParams, useSearchParams } from "react-router-dom";

// storageDetailPath is where a storage's detail opens: its drawer over the
// storage list, on a tab if one is named.
export function storageDetailPath(id: number | string, tab?: string): string {
  return `/storage?storage=${id}${tab ? `&tab=${encodeURIComponent(tab)}` : ""}`;
}

// StorageDetailRedirect keeps the old detail address (/storage/3?tab=files)
// working: it opens the same storage, on the same tab, in the list's drawer.
export function StorageDetailRedirect() {
  const { id } = useParams();
  const [params] = useSearchParams();
  return <Navigate replace to={storageDetailPath(id ?? "", params.get("tab") ?? undefined)} />;
}

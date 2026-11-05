// Vendor identities come from the API. The common locale dictionary is the
// only display-name mapping; a new server vendor remains readable to old UIs.
export function vendorName(value: string | undefined, t: (key: string) => string): string {
  const vendor = (value ?? "").trim().toLowerCase();
  if (!vendor) return "—";
  const key = `common:vendor.${vendor}`;
  const label = t(key);
  return label === key || label === `vendor.${vendor}` ? value! : label;
}

import type { TFunction } from "i18next";
import type { Storage } from "../../storageTypes";

/** Which nodes a storage applies to, in one short phrase: all, by label, or a number of chosen nodes. */
export function scopeText(t: TFunction, storage: Storage): string {
  if (storage.all_nodes) return t("scope.all", { ns: "storage" });
  const labels = storage.targets.filter((x) => x.label_key).map((x) => `${x.label_key}=${x.label_value ?? ""}`);
  const nodes = storage.targets.filter((x) => x.node_uuid && !x.override_only);
  const parts: string[] = [];
  if (labels.length > 0) parts.push(t("scope.labels", { ns: "storage", labels: labels.join(", ") }));
  if (nodes.length > 0) parts.push(t("scope.nodes", { ns: "storage", count: nodes.length }));
  return parts.join(" · ") || t("scope.none", { ns: "storage" });
}

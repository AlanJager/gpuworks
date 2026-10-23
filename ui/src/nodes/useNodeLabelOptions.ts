import { useMemo } from "react";
import { useTranslation } from "react-i18next";
import { systemLabelName, systemLabelValue } from "../components/SystemLabelTags";
import { useNodeFacets } from "./useNodeFacets";

/** A "key=value" label pick, as a select option with the nodes carrying it. */
export interface NodeLabelOption {
  value: string;
  label: string;
  nodes: number;
}

/**
 * useNodeLabelOptions lists the label values the fleet has, written labels
 * and the platform's own system labels (GET /nodes/facets), each as a
 * "key=value" pick with how many nodes carry it.
 */
export function useNodeLabelOptions() {
  const { t: tNodes } = useTranslation("nodes");
  const all = useNodeFacets({});
  const options = useMemo<NodeLabelOption[]>(() => {
    const data = all.data;
    if (!data) return [];
    const written = data.labels.flatMap(({ key, values }) =>
      values.map((item) => ({ value: `${key}=${item.value}`, label: `${key}=${item.value}`, nodes: item.nodes })),
    );
    const system = data.system_labels.flatMap(({ key, values }) =>
      values.map((item) => ({
        value: `${key}=${item.value}`,
        label: `${systemLabelName(key, tNodes)}: ${systemLabelValue({ key, value: item.value, display: item.display, display_code: item.display_code }, tNodes)}`,
        nodes: item.nodes,
      })),
    );
    return [...written, ...system];
  }, [all.data, tNodes]);
  return { options, total: all.data?.total, error: all.error };
}

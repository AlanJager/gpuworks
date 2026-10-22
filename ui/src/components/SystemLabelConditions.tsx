import { useMemo } from "react";
import { Form, Select } from "antd";
import { useTranslation } from "react-i18next";
import type { NodeFacets } from "../api";
import { systemLabelName, systemLabelValue } from "./SystemLabelTags";

// The keys a deploy form's hardware and network conditions offer. The node
// key is the manual pin and the vendor is the target vendor, each with its
// own control.
const SYSTEM_NODE_KEY = "gpuworks.ai/node";
const SYSTEM_VENDOR_KEY = "gpuworks.ai/vendor";

// systemConditionsToRequirements turns the control's "key=value" picks into
// node requirements: values of one key are alternatives.
export function systemConditionsToRequirements(values: unknown): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  if (!Array.isArray(values)) return out;
  for (const raw of values) {
    if (typeof raw !== "string") continue;
    const split = raw.indexOf("=");
    if (split <= 0) continue;
    const key = raw.slice(0, split);
    const value = raw.slice(split + 1);
    out[key] = [...(out[key] ?? []), value];
  }
  return out;
}

// SystemLabelConditions picks the hardware and network a deployment's
// candidate nodes must have, from the system labels the server generated for
// the nodes it has (GET /nodes/facets): the text shown is the server's
// display (no ID-to-name mapping here), each option with how many nodes
// carry it.
export default function SystemLabelConditions({ facets, disabled }: { facets?: NodeFacets["system_labels"]; disabled?: boolean }) {
  const { t } = useTranslation("deploy");
  const { t: tNodes } = useTranslation("nodes");
  const options = useMemo(
    () =>
      (facets ?? [])
        .filter(({ key }) => key !== SYSTEM_NODE_KEY && key !== SYSTEM_VENDOR_KEY)
        .map(({ key, values }) => ({
          label: systemLabelName(key, tNodes),
          options: values.map((value) => ({
            value: `${key}=${value.value}`,
            label: t("workbench.node.systemConditionOption", {
              key: systemLabelName(key, tNodes),
              value: systemLabelValue({ key, value: value.value, display: value.display, display_code: value.display_code }, tNodes),
              count: value.nodes,
            }),
          })),
        })),
    [facets, t, tNodes],
  );
  return (
    <Form.Item name="system_conditions" label={t("workbench.node.systemConditions")} extra={t("workbench.node.systemConditionsHelp")}>
      <Select mode="multiple" allowClear disabled={disabled} options={options} placeholder={t("workbench.node.systemConditionsPlaceholder")} optionFilterProp="label" />
    </Form.Item>
  );
}

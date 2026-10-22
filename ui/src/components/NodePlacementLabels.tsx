import { Alert, Form, Select, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { api, usePolling, type Node, type NodeFilter } from "../api";
import { useNodeFacets } from "../nodes/useNodeFacets";

function toLabels(values: string[]): Record<string, string> {
  return Object.fromEntries(values.map((value) => {
    const split = value.indexOf("=");
    return split < 0 ? [value, ""] : [value.slice(0, split), value.slice(split + 1)];
  }));
}

// matchedNames names the first few matching nodes; a pool of a thousand is
// a count, not a sentence.
export const NAMED_MATCHES = 5;
export function matchedNames(nodes: Node[], total = nodes.length): string {
  const names = nodes.slice(0, NAMED_MATCHES).map((node) => node.hostname || node.uuid).join(", ");
  return total > NAMED_MATCHES ? `${names}, …` : names;
}

/**
 * NodePlacementLabels picks the written labels a deployment's nodes must
 * carry, among the nodes `scope` describes. The choices and the matching
 * nodes come from the server - the label values the scope's nodes carry
 * (GET /nodes/facets) and one page of the nodes that carry the picked ones
 * (GET /nodes?label=…) - so the form never reads the whole fleet.
 */
export default function NodePlacementLabels({ scope }: { scope: NodeFilter }) {
  const { t } = useTranslation("nodes");
  const form = Form.useFormInstance();
  const labels = Form.useWatch("node_labels", { form, preserve: true }) as Record<string, string> | undefined;
  const facets = useNodeFacets(scope);
  const picked = Object.entries(labels ?? {}).map(([key, value]) => `${key}=${value}`);
  const scopeKey = JSON.stringify(scope);
  const matches = usePolling(
    () => (picked.length ? api.nodesPage({ page: 1, page_size: NAMED_MATCHES, ...scope, label: [...(scope.label ?? []), ...picked] }) : Promise.resolve(null)),
    0,
    [scopeKey, picked.join("\n")],
  );
  const options = (facets.data?.labels ?? []).flatMap(({ key, values }) => values.map(({ value }) => `${key}=${value}`));
  const total = matches.data?.total ?? 0;
  return <>
    <Form.Item name="node_labels" label={t("labels.placement")} extra={t("labels.placementHelp")}
      getValueProps={(value: Record<string, string> | undefined) => ({ value: Object.entries(value ?? {}).map(([key, val]) => `${key}=${val}`) })}
      normalize={toLabels}>
      <Select mode="tags" allowClear options={options.map((value) => ({ value, label: value }))} placeholder={t("labels.placeholder")} />
    </Form.Item>
    {picked.length > 0 && matches.data && (total === 0
      ? <Alert type="error" showIcon message={t("labels.noMatch")} />
      : <Typography.Paragraph type="secondary">{t("labels.matches", { count: total, nodes: matchedNames(matches.data.items, total) })}</Typography.Paragraph>)}
  </>;
}

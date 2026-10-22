import { useMemo } from "react";
import { Alert, Select, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { NodeRequirementsMap } from "../../../api";
import { systemLabelName, systemLabelValue } from "../../../components/SystemLabelTags";
import { useNodeFacets } from "../../../nodes/useNodeFacets";
import { picksToRequirements, requirementPicks } from "./pullFormat";

/**
 * NodeRequirementsSelect edits a node selector: the labels a node must carry
 * (values of one key are alternatives, every key must hold). The choices are
 * the label values the fleet has - written labels and the platform's own
 * system labels (GET /nodes/facets) - and the count under the control is the
 * server's own match, so what the operator sees is what the rule will cover.
 * An empty selector means every node.
 */
export default function NodeRequirementsSelect({
  value,
  onChange,
  id,
  ariaLabel,
}: {
  value: NodeRequirementsMap;
  onChange: (next: NodeRequirementsMap) => void;
  id?: string;
  ariaLabel: string;
}) {
  const { t } = useTranslation("settings");
  const { t: tNodes } = useTranslation("nodes");
  const picks = requirementPicks(value);
  const all = useNodeFacets({});
  const matched = useNodeFacets(picks.length ? { label: picks } : null);

  const options = useMemo(() => {
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

  const total = all.data?.total;
  const count = picks.length ? matched.data?.total : total;
  const failed = all.error || matched.error;

  return (
    <div>
      <Select
        id={id}
        mode="tags"
        allowClear
        aria-label={ariaLabel}
        style={{ width: "100%" }}
        value={picks}
        options={options}
        optionFilterProp="label"
        placeholder={t("pull.selector.placeholder")}
        tokenSeparators={[","]}
        onChange={(next: string[]) => onChange(picksToRequirements(next))}
        optionRender={(option) => (
          <span style={{ display: "inline-flex", gap: 8 }}>
            <span>{option.label}</span>
            <Typography.Text type="secondary">{t("pull.selector.optionNodes", { count: (option.data as { nodes?: number }).nodes ?? 0 })}</Typography.Text>
          </span>
        )}
      />
      <div aria-live="polite" style={{ marginTop: 4 }}>
        {failed ? (
          <Typography.Text type="secondary">{t("pull.selector.countUnknown")}</Typography.Text>
        ) : count === undefined ? null : picks.length > 0 && count === 0 ? (
          <Alert type="warning" showIcon message={t("pull.selector.noMatch")} />
        ) : (
          <Typography.Text type="secondary">
            {picks.length === 0 ? t("pull.selector.allNodes", { count }) : t("pull.selector.matches", { count, total })}
          </Typography.Text>
        )}
      </div>
      <Typography.Text type="secondary" style={{ display: "block", marginTop: 4, fontSize: 12 }}>
        {t("pull.selector.semantics")}
      </Typography.Text>
    </div>
  );
}

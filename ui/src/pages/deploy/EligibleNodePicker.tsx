import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";
import type { EligibleNode, Node } from "../../api";
import { nodeDisplayName, nodeRankAddress } from "../../nodeDisplay";
import NodePicker from "../../nodes/NodePicker";
import { placementRefusalText } from "./deploySchedule";

const ONLINE = { state: ["online"] };

// eligibility is evaluate's per-node judgement of the draft; before the first
// evaluation (or where nothing evaluates, as in the deployment detail) every
// node counts as eligible.
function eligibility(t: TFunction<"deploy">, eligibleNodes: EligibleNode[] | undefined, node: Node) {
  const judged = eligibleNodes?.find((item) => item.uuid === node.uuid);
  const reasons = (judged?.reasons ?? [])
    .map((reason) => placementRefusalText(t, reason))
    .filter(Boolean)
    .join(" · ");
  return { eligible: judged ? judged.eligible : true, reasons };
}

/**
 * EligibleNodePicker picks online nodes for a deployment draft, searched on
 * the server, rendering evaluate's eligibility answer: an ineligible node is
 * listed but disabled, its reasons in the option's title. A multi-node rank
 * (rankAddress) must also advertise an address the group can reach - its
 * compute network's IP, else its direct address.
 *
 * It works as an antd Form control.
 */
export default function EligibleNodePicker({
  eligibleNodes,
  rankAddress,
  ...control
}: {
  eligibleNodes?: EligibleNode[];
  rankAddress?: { suffix: string; hint?: string };
  value?: string | string[];
  onChange?: (value: string | string[] | undefined) => void;
  mode?: "multiple";
  maxCount?: number;
  disabled?: boolean;
  placeholder?: string;
  style?: React.CSSProperties;
  "aria-label"?: string;
}) {
  const { t } = useTranslation("deploy");
  const reachable = (node: Node) => !rankAddress || Boolean(nodeRankAddress(node));
  return (
    <NodePicker
      {...control}
      onChange={(value) => control.onChange?.(value)}
      filter={ONLINE}
      optionLabel={(node) => {
        const { eligible } = eligibility(t, eligibleNodes, node);
        const label = `${nodeDisplayName(node)} · ${node.gpu_count} GPU${eligible ? "" : t("workbench.node.chipMismatchSuffix")}`;
        return reachable(node) ? label : `${label} · ${rankAddress!.suffix}`;
      }}
      optionDisabled={(node) => !eligibility(t, eligibleNodes, node).eligible || !reachable(node)}
      optionTitle={(node) => {
        const { reasons } = eligibility(t, eligibleNodes, node);
        return reachable(node) ? reasons : [reasons, rankAddress!.hint].filter(Boolean).join(" · ");
      }}
    />
  );
}

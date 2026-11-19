import { forwardRef, type ComponentPropsWithoutRef } from "react";
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
  const texts = (judged?.reasons ?? [])
    .map((reason) => placementRefusalText(t, reason))
    .filter(Boolean);
  // first is why the node is turned down, as the option itself says it: the
  // reason the server gave, not a guess at its kind.
  return { eligible: judged ? judged.eligible : true, reasons: texts.join(" · "), first: texts[0] ?? "" };
}

type Props = {
  eligibleNodes?: EligibleNode[];
  rankAddress?: { suffix: string; hint?: string };
  value?: string | string[];
  onChange?: (value: string | string[] | undefined) => void;
  mode?: "multiple";
  maxCount?: number;
  disabled?: boolean;
  allowClear?: boolean;
  placeholder?: string;
  style?: React.CSSProperties;
  "aria-label"?: string;
} & Pick<ComponentPropsWithoutRef<typeof NodePicker>, "id" | "aria-invalid" | "aria-describedby" | "aria-required">;

/**
 * EligibleNodePicker picks online nodes for a deployment draft, searched on
 * the server, rendering evaluate's eligibility answer: an ineligible node is
 * listed but disabled, with the first reason it was turned down for beside
 * its name and all of them in the option's title. A multi-node rank
 * (rankAddress) must also advertise an address the group can reach - its
 * compute network's IP, else its direct address.
 *
 * It works as a form control.
 */
const EligibleNodePicker = forwardRef<HTMLInputElement, Props>(function EligibleNodePicker({
  eligibleNodes,
  rankAddress,
  ...control
}, ref) {
  const { t } = useTranslation("deploy");
  const reachable = (node: Node) => !rankAddress || Boolean(nodeRankAddress(node));
  return (
    <NodePicker
      ref={ref}
      {...control}
      onChange={(value) => control.onChange?.(value)}
      filter={ONLINE}
      optionLabel={(node) => {
        const { eligible, first } = eligibility(t, eligibleNodes, node);
        const label = `${nodeDisplayName(node)} · ${node.gpu_count} GPU${eligible || !first ? "" : ` · ${first}`}`;
        return reachable(node) ? label : `${label} · ${rankAddress!.suffix}`;
      }}
      optionDisabled={(node) => !eligibility(t, eligibleNodes, node).eligible || !reachable(node)}
      optionTitle={(node) => {
        const { reasons } = eligibility(t, eligibleNodes, node);
        return reachable(node) ? reasons : [reasons, rankAddress!.hint].filter(Boolean).join(" · ");
      }}
    />
  );
});

export default EligibleNodePicker;

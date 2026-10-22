import type { TFunction } from "i18next";

import type { EvaluateResponse } from "../../api";
import { placementRefusalText } from "./deploySchedule";

// A node the automatic placement looked at and turned down, with the reason
// the scheduler gave, already in the viewer's language.
export interface NodeRefusal {
  uuid: string;
  name: string;
  reason: string;
}

// Reasons the eligibility pass gives before any plan is tried: the node is
// offline, cordoned, off the template's chip or outside the node
// requirements. They say less about this deployment than a refused plan does,
// so they list after the nodes that were planned.
const EligibilityCodes = new Set([
  "ineligible",
  "node_offline",
  "node_cordoned",
  "node_requirements_unmet",
  "node_requirements_invalid",
  "vendor_mismatch",
  "chip_mismatch",
]);

type Candidate = NonNullable<EvaluateResponse["candidates"]>[number];

function eligibilityOnly(candidate: Candidate): boolean {
  const reason = candidate.reason;
  return typeof reason === "object" && reason !== null && EligibilityCodes.has(reason.code ?? "");
}

/**
 * The nodes an evaluation turned down for the draft. Empty when the server
 * judged no node at all (nothing online matches) or when a node was selected.
 */
export function nodeRefusals(
  t: TFunction<"deploy">,
  result: EvaluateResponse | null | undefined,
): NodeRefusal[] {
  if (!result || result.selected_node) return [];
  return (result.candidates ?? [])
    .filter((candidate) => !candidate.ok)
    .map((candidate, index) => ({ candidate, index }))
    .sort((a, b) => Number(eligibilityOnly(a.candidate)) - Number(eligibilityOnly(b.candidate)) || a.index - b.index)
    .map(({ candidate }) => ({
      uuid: candidate.node_uuid,
      name: candidate.node_name || candidate.node_uuid,
      reason: placementRefusalText(t, candidate.reason) || placementRefusalText(t, candidate.plan?.message),
    }));
}

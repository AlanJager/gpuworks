import type { Node } from "../api";
import { recoveryLabel } from "./recoveryPhase";

/**
 * How a node's connection reads, and nothing else about it. A node's state
 * says whether the control plane has its agent on the other end; what its
 * accelerators are and whether they can be used are the cards' own facts.
 *
 * Out-of-band recovery is part of the same story rather than a parallel
 * one, and the server has already folded it in: a node whose agent is being
 * restored arrives as connecting. The phase is detail for a tooltip here,
 * never a second status to read.
 */
export type NodeConnection = {
  /** Which of the three states to show. */
  state: "online" | "offline" | "connecting";
  /** i18n key for the state's own label. */
  labelKey: string;
  badge: "success" | "default" | "processing";
  /**
   * i18n key for what the recovery is doing, or why it stopped; undefined
   * when there is nothing to add. Rendered as a warning mark with the detail
   * on hover, never as a state of its own.
   */
  noteKey?: string;
};

export function nodeConnection(node: Pick<Node, "state" | "recovery_phase">): NodeConnection {
  const noteKey = recoveryLabel[node.recovery_phase ?? ""]?.key;
  switch (node.state) {
    case "online":
      return { state: "online", labelKey: "state.online", badge: "success" };
    case "connecting":
      return { state: "connecting", labelKey: "state.connecting", badge: "processing", noteKey };
    default:
      return { state: "offline", labelKey: "state.offline", badge: "default", noteKey };
  }
}

/** The three states a node list can be filtered by, in display order. */
export const NODE_CONNECTION_STATES = ["online", "connecting", "offline"] as const;

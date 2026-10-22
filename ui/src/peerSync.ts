/** The node states hasLiveAgent accepts, as a node list filter. */
export const LIVE_AGENT_STATES = ["online"];

/**
 * Whether a node can take part in a direct peer copy of a model or image, as
 * source or target. It only needs a live agent session; moving a copy needs
 * no accelerator, so a node whose cards are in trouble still qualifies —
 * that trouble is the cards' own state and never takes the node offline.
 */
export function hasLiveAgent(state: string) {
  return LIVE_AGENT_STATES.includes(state);
}

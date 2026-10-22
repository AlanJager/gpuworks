// Card tests: what little the client decides for itself. How large a test is
// and what shape its result has are the server's to say (tests/estimate,
// card_shape); this only tells the kinds apart and reads one card pair.
import type { NetworkTestKind, NetworkTestRail } from "../../networkTypes";

export function isCardKind(kind: NetworkTestKind | undefined): kind is "card_ping" | "card_matrix" {
  return kind === "card_ping" || kind === "card_matrix";
}

export function cardOK(r: NetworkTestRail) {
  return !r.code && !r.error && !!r.reachable;
}

import type { NetworkTestKind, NetworkTestRail } from "../../networkTypes";

export function isCardKind(kind: NetworkTestKind | undefined): kind is "card_ping" | "card_matrix" {
  return kind === "card_ping" || kind === "card_matrix";
}

export function cardOK(rail: NetworkTestRail) {
  return rail.passed;
}

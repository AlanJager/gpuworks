import { productMessageText, type APIErrorTranslator, type ProductMessage } from "./apiError";

/**
 * runtimeFailureText renders an instance's runtime failure (the server's
 * proto.RuntimeFailure as a product message) in the reader's language: the
 * sentence for its code, the known failures the output showed, the engine's
 * root-cause line when the output had one, and the rank it happened on when
 * it ended a multi-node generation. t must resolve the common namespace and
 * the ai namespace's finding titles ("ai:finding.<CODE>.title").
 */
// A line that names an error, as the agent picks an output's root cause
// (pkg/enginelog errorLineRE). The failure's evidence is either that one
// line, or, when the output named no error, its last lines oldest first
// (internal/agent/engine exitOutput.attach); the wire does not say which, the
// line itself does.
const errorLine = /(^|[\s:])[a-z_.]*(error|exception)\b|\berror:/i;

export function runtimeFailureText(t: APIErrorTranslator, failure: ProductMessage): string {
  const params = failure.params ?? {};
  let text = productMessageText(t, failure);
  const causes = (Array.isArray(params.causes) ? params.causes : []) as ProductMessage[];
  const titles = causes
    .filter((cause) => cause && typeof cause.code === "string")
    .map((cause) => t(`ai:finding.${cause.code}.title`, { ...cause.params, defaultValue: cause.message || cause.code }));
  if (titles.length > 0) text = t("runtimeFailure.causes", { text, causes: titles, defaultValue: `${text} (${titles.join(", ")})` });
  const lines = Array.isArray(params.evidence) ? params.evidence : [];
  // The newest line is where the process stopped; the agent's own message
  // ends with the same one. It is called an error only when it names one.
  const evidence = String(lines[lines.length - 1] ?? "");
  if (evidence) {
    const key = errorLine.test(evidence) ? "runtimeFailure.evidence" : "runtimeFailure.lastOutput";
    text = t(key, { text, evidence, defaultValue: `${text}: ${evidence}` });
  }
  if (typeof params.rank === "number" && !failure.code.startsWith("UNIT_RANK_")) {
    text = t("runtimeFailure.rank", { rank: params.rank, node: params.node ?? "", text, defaultValue: text });
  }
  return text;
}

/** isProductMessage says a wire value (an event param) is a product message. */
export function isProductMessage(value: unknown): value is ProductMessage {
  return typeof value === "object" && value !== null && typeof (value as ProductMessage).code === "string" && typeof (value as ProductMessage).message === "string";
}

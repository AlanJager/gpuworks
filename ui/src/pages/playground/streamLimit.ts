// A streamed reply holds its HTTP connection until it ends. Over HTTP/1.1 a
// browser keeps at most six connections per server, so more than four
// replies at once would queue the page's other requests (and the queueing
// would count into TTFT). HTTP/2 and HTTP/3 multiplex one connection; there
// the cap only keeps the comparison meaningful.
export const HTTP1_STREAM_LIMIT = 4;
export const MULTIPLEXED_STREAM_LIMIT = 12;

export interface StreamLimit {
  limit: number;
  multiplexed: boolean;
}

/** pageProtocol is the protocol the browser negotiated for this page, e.g. "h2" or "http/1.1". */
export function pageProtocol(): string {
  try {
    const entry = performance.getEntriesByType?.("navigation")[0] as PerformanceNavigationTiming | undefined;
    return entry?.nextHopProtocol ?? "";
  } catch {
    return "";
  }
}

export function streamLimit(protocol: string = pageProtocol()): StreamLimit {
  const multiplexed = protocol === "h2" || protocol === "h3" || protocol.startsWith("h3-");
  return { limit: multiplexed ? MULTIPLEXED_STREAM_LIMIT : HTTP1_STREAM_LIMIT, multiplexed };
}

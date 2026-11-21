export interface ConnectionCheckEvent {
  seq: number;
  name: string;
  status: "ok" | "failed" | "skipped";
  duration_ms: number;
  code?: string;
  detail?: Record<string, unknown>;
  hint?: string;
}

export interface ConnectionTestSummary {
  ok: boolean;
  /** The failed checks, in report order. */
  failed?: string[];
  /** Present instead of `failed` when the test could not run at all: a product error code. */
  code?: string;
}

export interface ProviderTestRequest {
  kind: string;
  endpoint?: string;
  api_key?: string;
  public_key?: string;
  private_key?: string;
  extra?: Record<string, string>;
}

/** What a test of a saved account may override: a field left out means the stored value. */
export type ProviderTestOverride = Omit<ProviderTestRequest, "kind">;

/** The stream of a connection test broke before its summary: the connection dropped, or the server never answered. */
export class ConnectionTestInterrupted extends Error {
  constructor() {
    super("the connection test stream ended before its summary");
    this.name = "ConnectionTestInterrupted";
  }
}

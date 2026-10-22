import { notifySessionExpired } from "./session";

export interface APIErrorPayload {
  error?: string;
  message?: string;
  code?: string;
  params?: Record<string, unknown>;
}

export type APIErrorTranslator = (key: string, options: Record<string, unknown>) => string;

export class APIError extends Error {
  readonly code: string;
  readonly params?: Record<string, unknown>;
  readonly fallback: string;
  readonly status: number;

  constructor(code: string, params: Record<string, unknown> | undefined, fallback: string, status: number) {
    super(fallback || code);
    this.name = "APIError";
    this.code = code;
    this.params = params;
    this.fallback = fallback;
    this.status = status;
  }
}

export function apiErrorText(t: APIErrorTranslator, error: unknown): string {
  if (!(error instanceof APIError)) return error instanceof Error ? error.message : String(error ?? "");
  return t(`apiError.${error.code}`, {
    ...error.params,
    defaultValue: error.fallback || error.code,
  });
}

/** A product message an answer carries (dto.ProductMessage), in the reader's language. */
export interface ProductMessage {
  code: string;
  params?: Record<string, unknown>;
  message: string;
}

export function productMessageText(t: APIErrorTranslator, message: ProductMessage): string {
  return apiErrorText(t, new APIError(message.code, message.params, message.message, 200));
}

// ProxyErrorPage is an HTML page in place of a gpuworks JSON error: a reverse
// proxy, CDN or tunnel in front of the server answered, or replaced the
// server's own answer (Cloudflare does that to a 502 or 504), so the reason
// the server gave is gone.
export class ProxyErrorPage extends Error {
  readonly status: number;

  constructor(status: number, statusText: string) {
    super(`HTTP ${status}${statusText ? ` ${statusText}` : ""}: a reverse proxy or tunnel in front of gpuworks answered with its own error page; the server's reason is not available`);
    this.name = "ProxyErrorPage";
    this.status = status;
  }
}

function looksLikeHTML(response: Response, raw: string): boolean {
  return (response.headers.get("Content-Type") ?? "").toLowerCase().includes("text/html") || /^\s*<(!doctype|html)/i.test(raw);
}

export async function apiResponseError(response: Response, fallback: string): Promise<Error> {
  let raw = "";
  try {
    raw = await response.text();
  } catch {
    // Keep the request/status fallback when a response body cannot be read.
  }
  let payload: APIErrorPayload | undefined;
  if (raw && looksLikeHTML(response, raw)) return new ProxyErrorPage(response.status, response.statusText);
  if (raw) {
    try {
      payload = JSON.parse(raw) as APIErrorPayload;
    } catch {
      // Non-JSON responses remain raw lower-level errors.
    }
  }
  const detail = payload?.error || payload?.message || raw || fallback;
  if (response.status === 401 && payload?.code === "AUTHENTICATION_REQUIRED") notifySessionExpired();
  if (payload?.code) return new APIError(payload.code, payload.params, detail, response.status);
  return new Error(detail);
}

// HTML preview of playground replies. The isolation model is ADR 0037:
// model output runs in an opaque-origin sandbox that never gets the console's
// session, and, unless an administrator allows it, a CSP keeps it from making
// network requests from the viewer's browser.

/** Fence languages that get a preview button. */
export const PREVIEW_LANGUAGES = new Set(["html", "htm", "xhtml", "svg"]);

/**
 * The iframe sandbox, identical under both network policies. Never add
 * allow-same-origin: with allow-scripts it would hand the page the console's
 * origin, its cookies and its API. No allow-popups/top-navigation/forms
 * either; links are opened by the parent (see the bootstrap script).
 */
export const PREVIEW_SANDBOX = "allow-scripts allow-modals allow-downloads";

/**
 * The restrictive policy: scripts, styles, images, fonts and media may come
 * inline or from HTTPS (CDN libraries), but no fetch/XHR/WebSocket/EventSource/
 * beacon and nothing over plain http. eval and wasm stay allowed: inline
 * script already is, so forbidding them protects nothing and breaks Vue's
 * in-browser compiler, Alpine.js and wasm pages.
 */
export const RESTRICTED_CSP = [
  "default-src 'none'",
  "script-src 'unsafe-inline' 'unsafe-eval' 'wasm-unsafe-eval' https:",
  "style-src 'unsafe-inline' https:",
  "img-src data: blob: https:",
  "font-src data: https:",
  "media-src data: blob: https:",
  "worker-src blob:",
  "connect-src 'none'",
  "form-action 'none'",
  "base-uri 'none'",
].join("; ");

/** Marks the bootstrap script's messages to the parent. */
export const PREVIEW_MESSAGE_SOURCE = "gpuworks-preview";

export type PreviewMessage =
  | { type: "blocked"; directive: string; uri: string }
  | { type: "open"; url: string };

/**
 * looksLikeHTMLDocument reports a reply that is itself an HTML document
 * rather than Markdown with a fenced block.
 */
export function looksLikeHTMLDocument(content: string): boolean {
  return /^(<!doctype\s+html|<html[\s>])/i.test(content.trimStart());
}

/**
 * bootstrapScript runs before any page script. It gives storage and cookies an
 * in-memory stand-in (an opaque origin throws SecurityError on them, which
 * breaks many generated pages outright), hands http(s) link clicks to the
 * parent instead of navigating the frame, and, under the restrictive policy,
 * reports CSP violations so the parent can say what was blocked.
 */
function bootstrapScript(reportViolations: boolean): string {
  return `(() => {
  const post = (data) => { try { parent.postMessage(Object.assign({ source: ${JSON.stringify(PREVIEW_MESSAGE_SOURCE)} }, data), "*"); } catch (_) {} };
  const memoryStorage = () => {
    const items = new Map();
    return {
      get length() { return items.size; },
      key: (index) => Array.from(items.keys())[index] ?? null,
      getItem: (key) => items.has(String(key)) ? items.get(String(key)) : null,
      setItem: (key, value) => { items.set(String(key), String(value)); },
      removeItem: (key) => { items.delete(String(key)); },
      clear: () => { items.clear(); },
    };
  };
  for (const name of ["localStorage", "sessionStorage"]) {
    try { void window[name]; } catch (_) {
      try { Object.defineProperty(window, name, { configurable: true, value: memoryStorage() }); } catch (_) {}
    }
  }
  try { void document.cookie; } catch (_) {
    const jar = new Map();
    try {
      Object.defineProperty(document, "cookie", {
        configurable: true,
        get: () => Array.from(jar.values()).join("; "),
        set: (value) => { const pair = String(value).split(";")[0].trim(); const name = pair.split("=")[0].trim(); if (name) jar.set(name, pair); },
      });
    } catch (_) {}
  }
  window.addEventListener("click", (event) => {
    if (event.defaultPrevented || !(event.target instanceof Element)) return;
    const anchor = event.target.closest("a[href]");
    const href = anchor ? anchor.getAttribute("href") : "";
    if (!href || href.startsWith("#") || anchor.hasAttribute("download")) return;
    let url;
    try { url = new URL(href, document.baseURI); } catch (_) { return; }
    if (url.protocol !== "http:" && url.protocol !== "https:") return;
    event.preventDefault();
    post({ type: "open", url: url.href });
  });
  ${reportViolations ? `document.addEventListener("securitypolicyviolation", (event) => post({ type: "blocked", directive: event.effectiveDirective, uri: event.blockedURI }));` : ""}
})();`;
}

/**
 * buildPreviewDocument turns model output (a full document, a fragment or a
 * bare <svg>) into the srcdoc of the preview frame. DOMParser completes a
 * fragment into a document without running anything; our charset, CSP and
 * bootstrap go first in <head>, before any of the page's own elements. A CSP
 * meta the page adds later can only narrow the policy, never widen it.
 */
export function buildPreviewDocument(source: string, { allowNetwork }: { allowNetwork: boolean }): string {
  const doc = new DOMParser().parseFromString(source, "text/html");
  const charset = doc.createElement("meta");
  charset.setAttribute("charset", "utf-8");
  const injected: Element[] = [charset];
  if (!allowNetwork) {
    const csp = doc.createElement("meta");
    csp.setAttribute("http-equiv", "Content-Security-Policy");
    csp.setAttribute("content", RESTRICTED_CSP);
    injected.push(csp);
  }
  const script = doc.createElement("script");
  script.textContent = bootstrapScript(!allowNetwork);
  injected.push(script);
  doc.head.prepend(...injected);
  return `<!doctype html>\n${doc.documentElement.outerHTML}`;
}

/**
 * parsePreviewMessage validates a message from the frame. The page can post
 * anything, so only the two known shapes pass, with string fields.
 */
export function parsePreviewMessage(data: unknown): PreviewMessage | undefined {
  if (!data || typeof data !== "object") return undefined;
  const value = data as Record<string, unknown>;
  if (value.source !== PREVIEW_MESSAGE_SOURCE) return undefined;
  if (value.type === "blocked" && typeof value.directive === "string" && typeof value.uri === "string") {
    return { type: "blocked", directive: value.directive, uri: value.uri };
  }
  if (value.type === "open" && typeof value.url === "string") return { type: "open", url: value.url };
  return undefined;
}

/** blockedTarget names what a violation tried to reach: the host of a URL, else the raw value. */
export function blockedTarget(uri: string): string {
  try {
    const url = new URL(uri);
    return url.host || url.protocol;
  } catch {
    return uri.slice(0, 80);
  }
}

/** linkToOpen accepts only http(s) URLs from the frame's open requests. */
export function linkToOpen(raw: string): string | undefined {
  try {
    const url = new URL(raw);
    return url.protocol === "http:" || url.protocol === "https:" ? url.href : undefined;
  } catch {
    return undefined;
  }
}

import assert from "node:assert/strict";
import { register } from "node:module";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";

register(new URL("./deploySleepableCssHook.mjs", import.meta.url));
// Vite exposes these CommonJS browser packages as named exports. Preserve the
// real classes while supplying that same interop to Node's native ESM loader.
register("data:text/javascript," + encodeURIComponent(`
  export async function load(url, context, next) {
    const name = url.endsWith("/@xterm/xterm/lib/xterm.js") ? "Terminal"
      : url.endsWith("/@xterm/addon-fit/lib/addon-fit.js") ? "FitAddon" : null;
    if (name) return { format: "module", shortCircuit: true,
      source: 'import implementation from ' + JSON.stringify(url + "?commonjs")
        + '; export const ' + name + ' = implementation.' + name + ';' };
    return next(url, context);
  }
`));
Object.defineProperty(globalThis, "self", { value: dom.window, configurable: true });
Object.defineProperty(globalThis, "requestAnimationFrame", { value: (callback: () => void) => setTimeout(callback, 0), configurable: true });
Object.defineProperty(globalThis, "cancelAnimationFrame", { value: clearTimeout, configurable: true });
Object.defineProperty(dom.window, "requestAnimationFrame", { value: globalThis.requestAnimationFrame, configurable: true });
Object.defineProperty(dom.window, "cancelAnimationFrame", { value: globalThis.cancelAnimationFrame, configurable: true });
// xterm renders canvas glyphs; JSDOM supplies the surrounding real terminal DOM.
const context = new Proxy({}, { get: (_target, key) => key === "measureText" ? () => ({ width: 8 }) : () => undefined });
dom.window.HTMLCanvasElement.prototype.getContext = () => context as CanvasRenderingContext2D;

const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const NodeTerminal = (await import("../src/components/NodeTerminal.tsx")).default;
const originalFetch = globalThis.fetch;
const originalWebSocket = globalThis.WebSocket;

afterEach(() => {
  cleanup();
  globalThis.fetch = originalFetch;
  globalThis.WebSocket = originalWebSocket;
});

for (const language of ["en", "zh"] as const) {
  test(`a reader's real terminal preflight 403 uses the ${language} product error and never opens a socket`, async () => {
    const { i18n, locale } = await createI18n(["nodes", "common"], language);
    const requests: Array<{ url: string; init?: RequestInit }> = [];
    let sockets = 0;
    globalThis.fetch = async (input, init) => {
      requests.push({ url: String(input), init });
      return new Response(JSON.stringify({ error: "administrator role required", code: "ADMINISTRATOR_REQUIRED" }), {
        status: 403, headers: { "Content-Type": "application/json" },
      });
    };
    globalThis.WebSocket = class {
      constructor() { sockets++; throw new Error("a denied terminal must not attempt execution"); }
    } as unknown as typeof WebSocket;

    render(<I18nextProvider i18n={i18n}><NodeTerminal nodeUUID="node/denied" target="container" ref_="container-private" /></I18nextProvider>);
    const alert = await screen.findByRole("alert");
    await waitFor(() => assert.equal(alert.textContent, locale.common.apiError.ADMINISTRATOR_REQUIRED));
    assert.ok(!alert.textContent?.includes("administrator role required"), "the backend English fallback must not replace translated text");
    assert.deepEqual(requests, [{ url: "/api/v1/nodes/node%2Fdenied/terminal/settings", init: { credentials: "include" } }]);
    assert.equal(sockets, 0, "authorization rejection must happen before any WebSocket or container reference is sent");
    assert.ok(!dom.window.document.body.textContent?.includes("container-private"));
  });
}

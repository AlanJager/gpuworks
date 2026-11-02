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

// Literal current server wire: terminalSettings returns HTTP200 for capacity
// denial; unlike the 403 test above there is no ProductError code to translate.
test("a Chinese host preflight 200 node-limit denial is localized before any socket opens", async () => {
  const { i18n } = await createI18n(["nodes", "common"], "zh");
  const requests: string[] = [];
  let sockets = 0;
  globalThis.fetch = async (input, init) => {
    assert.equal(String(input), "/api/v1/nodes/owned-host/terminal/settings");
    assert.deepEqual(init, { credentials: "include" });
    requests.push(String(input));
    return new Response(JSON.stringify({
      enabled: true, idle_timeout_min: 15, max_per_node: 1, max_global: 2,
      active_total: 1, active_node: 1, can_open: false,
      reason: "too many terminal sessions on this node",
      container_supported: false,
      container_reason: "this node's agent does not support container terminals",
      container_runtime: "unknown", container_capability: "webterm.container",
    }), { status: 200, headers: { "Content-Type": "application/json" } });
  };
  globalThis.WebSocket = class {
    constructor() { sockets++; throw new Error("preflight denial must not open a WebSocket"); }
  } as unknown as typeof WebSocket;
  render(<I18nextProvider i18n={i18n}><NodeTerminal nodeUUID="owned-host" /></I18nextProvider>);
  const alert = await screen.findByRole("alert");
  await waitFor(() => assert.equal(alert.textContent, "此节点的终端会话数已达上限。"));
  assert.equal(sockets, 0);
  assert.deepEqual(requests, ["/api/v1/nodes/owned-host/terminal/settings"]);
});

const allowedPreflight = {
  enabled: true, idle_timeout_min: 15, max_per_node: 4, max_global: 32,
  active_total: 0, active_node: 0, can_open: true, reason: "",
  container_supported: true, container_reason: "", container_runtime: "docker",
  container_capability: "webterm.container",
};

function denyPreflight(body: typeof allowedPreflight) {
  const requests: string[] = [];
  let sockets = 0;
  globalThis.fetch = async (input, init) => {
    assert.equal(String(input), "/api/v1/nodes/owned-host/terminal/settings", "unexpected fixture API");
    assert.deepEqual(init, { credentials: "include" });
    requests.push(String(input));
    return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
  };
  globalThis.WebSocket = class {
    constructor() { sockets++; throw new Error("preflight denial must not create a socket"); }
  } as unknown as typeof WebSocket;
  return () => {
    assert.equal(sockets, 0);
    assert.deepEqual(requests, ["/api/v1/nodes/owned-host/terminal/settings"]);
  };
}

// Exact current terminalSettings/webterm literals, without invented error codes.
const hostReasons = [
  { reason: "terminal streaming is not available", zh: "终端服务不可用。", fields: {} },
  { reason: "web terminal is disabled in platform settings", zh: "平台设置已禁用 Web 终端。", fields: { enabled: false } },
  { reason: "node is not connected", zh: "节点未连接。", fields: {} },
  { reason: "this node's agent does not support terminals; upgrade the agent", zh: "此节点的代理不支持终端，请升级代理。", fields: {} },
  { reason: "too many terminal sessions on this node", zh: "此节点的终端会话数已达上限。", fields: { active_node: 4, active_total: 4 } },
  { reason: "too many terminal sessions on this platform", zh: "平台终端会话数已达上限。", fields: { active_total: 32 } },
];
const containerReasons = [
  { reason: "this node's agent does not support container terminals", zh: "此节点的代理不支持容器终端。", runtime: "docker", capability: "webterm.container" },
  { reason: "runveil container shell is unavailable on this agent", zh: "此节点的代理无法使用 runveil 容器 Shell。", runtime: "udocker", capability: "webterm.container.udocker" },
];

for (const language of ["en", "zh"] as const) {
  for (const entry of hostReasons) {
    test(`${language} HTTP200 host denial: ${entry.reason}`, async () => {
      const { i18n } = await createI18n(["nodes", "common"], language);
      const verify = denyPreflight({ ...allowedPreflight, ...entry.fields, can_open: false, reason: entry.reason });
      render(<I18nextProvider i18n={i18n}><NodeTerminal nodeUUID="owned-host" /></I18nextProvider>);
      const alert = await screen.findByRole("alert");
      await waitFor(() => assert.equal(alert.textContent, language === "zh" ? entry.zh : entry.reason));
      verify();
    });
  }
  for (const entry of containerReasons) {
    test(`${language} HTTP200 container capability denial: ${entry.runtime}`, async () => {
      const { i18n } = await createI18n(["nodes", "common"], language);
      const verify = denyPreflight({ ...allowedPreflight, container_supported: false,
        container_reason: entry.reason, container_runtime: entry.runtime, container_capability: entry.capability });
      render(<I18nextProvider i18n={i18n}><NodeTerminal nodeUUID="owned-host" target="container" ref_="owned-container" /></I18nextProvider>);
      const alert = await screen.findByRole("alert");
      await waitFor(() => assert.equal(alert.textContent, language === "zh" ? entry.zh : entry.reason));
      assert.equal(screen.getAllByRole("alert").length, 1, "blocked runveil must not show the normal runtime notice");
      verify();
    });
  }
  for (const target of ["host", "container"] as const) {
    test(`${language} preserves an unknown ${target} preflight diagnostic verbatim`, async () => {
      const { i18n } = await createI18n(["nodes", "common"], language);
      const diagnostic = target === "host" ? "node is not connected: relay R42 (retry in 7s)"
        : "runveil container shell is unavailable on this agent: relay R42";
      const verify = denyPreflight({ ...allowedPreflight,
        ...(target === "host" ? { can_open: false, reason: diagnostic }
          : { container_supported: false, container_reason: diagnostic, container_runtime: "udocker", container_capability: "webterm.container.udocker" }) });
      render(<I18nextProvider i18n={i18n}><NodeTerminal nodeUUID="owned-host" target={target} ref_={target === "container" ? "owned-container" : undefined} /></I18nextProvider>);
      const alert = await screen.findByRole("alert");
      await waitFor(() => assert.equal(alert.textContent, diagnostic));
      verify();
    });
  }
}

for (const language of ["en", "zh"] as const) {
  for (const target of ["host", "container"] as const) {
    test(`${language} admitted ${target} retains socket setup and unmount close`, async () => {
      const { i18n } = await createI18n(["nodes", "common"], language);
      const requests: string[] = [];
      const sockets: SocketBoundary[] = [];
      globalThis.fetch = async (input, init) => {
        requests.push(String(input));
        assert.deepEqual(init, { credentials: "include" });
        const url = String(input);
        assert.ok(url === "/api/v1/nodes/owned-host/terminal/settings" || url === "/api/v1/auth/me", "unexpected admitted fixture API");
        return new Response(JSON.stringify(url.endsWith("/settings") ? {
          ...allowedPreflight, container_runtime: "udocker", container_capability: "webterm.container.udocker",
        } : { username: "admin", role: "admin" }), { status: 200, headers: { "Content-Type": "application/json" } });
      };
      // Only external WebSocket IO is replaced; real terminal/component code runs.
      class SocketBoundary {
        static OPEN = 1;
        static CONNECTING = 0;
        readyState = SocketBoundary.CONNECTING;
        binaryType = "blob";
        onopen: (() => void) | null = null;
        onclose: ((event: { wasClean: boolean }) => void) | null = null;
        closed = false;
        constructor(readonly url: string) {
          sockets.push(this);
          queueMicrotask(() => { if (!this.closed) { this.readyState = SocketBoundary.OPEN; this.onopen?.(); } });
        }
        send(data: unknown) {
          assert.equal(this.readyState, SocketBoundary.OPEN);
          assert.ok(data instanceof Uint8Array || typeof data === "string");
        }
        close() { this.closed = true; this.readyState = 3; this.onclose?.({ wasClean: true }); }
      }
      globalThis.WebSocket = SocketBoundary as unknown as typeof WebSocket;
      const rendered = render(<I18nextProvider i18n={i18n}><NodeTerminal nodeUUID="owned-host" target={target} ref_={target === "container" ? "owned-container" : undefined} /></I18nextProvider>);
      await screen.findByText(language === "zh" ? "已连接" : "Connected", { exact: true });
      assert.equal(sockets.length, 1);
      const url = new URL(sockets[0].url);
      assert.equal(url.pathname, "/api/v1/nodes/owned-host/terminal");
      assert.equal(url.searchParams.get("target"), target);
      assert.equal(url.searchParams.get("ref"), target === "container" ? "owned-container" : null);
      assert.ok(Number(url.searchParams.get("rows")) > 0 && Number(url.searchParams.get("cols")) > 0);
      if (target === "container") {
        assert.ok(screen.getByRole("alert").textContent?.startsWith(language === "zh" ? "runveil 容器 Shell：" : "runveil shell:"));
      } else {
        assert.equal(screen.queryByRole("alert"), null);
      }
      rendered.unmount();
      await waitFor(() => assert.equal(sockets[0].closed, true));
      assert.deepEqual(requests, ["/api/v1/nodes/owned-host/terminal/settings", "/api/v1/auth/me"]);
    });
  }
}

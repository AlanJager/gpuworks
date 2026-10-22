import assert from "node:assert/strict";
import test from "node:test";

import { APIError, apiErrorText, apiResponseError, ProxyErrorPage } from "../src/apiError.ts";

test("structured API errors translate by code with params", async () => {
  const response = new Response(JSON.stringify({
    error: "English fallback",
    code: "EXAMPLE_ERROR",
    params: { resource: "gpu-1" },
  }), { status: 409 });
  const error = await apiResponseError(response, "request failed");
  assert.ok(error instanceof APIError);
  assert.equal(error.status, 409);
  assert.equal(apiErrorText((key, options) => `${key}:${options.resource}`, error), "apiError.EXAMPLE_ERROR:gpu-1");
});

test("API error parsing preserves old JSON and raw response bodies", async () => {
  const legacy = await apiResponseError(new Response('{"error":"legacy error"}', { status: 400 }), "request failed");
  assert.equal(legacy.message, "legacy error");
  assert.equal(apiErrorText(() => "unexpected", legacy), "legacy error");

  const raw = await apiResponseError(new Response("upstream failed", { status: 502 }), "request failed");
  assert.equal(raw.message, "upstream failed");
});

test("missing API error translations use the English fallback", () => {
  const error = new APIError("FUTURE_CODE", undefined, "English fallback", 500);
  assert.equal(apiErrorText((_key, options) => String(options.defaultValue), error), "English fallback");
});

test("an HTML error page from a proxy or tunnel never becomes the error text", async () => {
  // What Cloudflare serves in place of an origin 502, shortened.
  const page = '<!DOCTYPE html>\n<html class="no-js" lang="en-US"><head><title>trycloudflare.com | 502: Bad gateway</title></head><body>...</body></html>';
  const error = await apiResponseError(new Response(page, { status: 502, statusText: "Bad Gateway", headers: { "Content-Type": "text/html; charset=UTF-8" } }), "request failed");
  assert.ok(error instanceof ProxyErrorPage);
  assert.equal(error.status, 502);
  assert.ok(!error.message.includes("<"), error.message);
  assert.match(error.message, /^HTTP 502 Bad Gateway: a reverse proxy or tunnel/);

  // Without a Content-Type, the markup itself still gives the page away.
  const untyped = await apiResponseError(new Response(page, { status: 502 }), "request failed");
  assert.ok(untyped instanceof ProxyErrorPage);

  // gpuworks JSON errors and plain-text bodies are unaffected.
  const json = await apiResponseError(new Response('{"error":"the target has not authorized the platform key"}', { status: 422, headers: { "Content-Type": "application/json" } }), "request failed");
  assert.equal(json.message, "the target has not authorized the platform key");
});

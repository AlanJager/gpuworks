import assert from "node:assert/strict";
import test from "node:test";
import { playgroundErrorText } from "../src/pages/playground/api.ts";

test("a FastAPI 404 is a product sentence, not the raw body", () => {
  assert.equal(playgroundErrorText(404, `{"detail":"Not Found"}`), "This deployment does not serve the requested route");
  const previous = globalThis.document;
  Object.defineProperty(globalThis, "document", {
    configurable: true,
    value: { documentElement: { lang: "zh-CN" } },
  });
  try {
    assert.equal(playgroundErrorText(404, `{"detail":"Not Found"}`), "这次部署没有提供所请求的路由");
  } finally {
    if (previous === undefined) Reflect.deleteProperty(globalThis, "document");
    else Object.defineProperty(globalThis, "document", { configurable: true, value: previous });
  }
  assert.equal(playgroundErrorText(400, `{"error":{"message":"bad input"}}`), "bad input");
  assert.equal(playgroundErrorText(404, `{"detail":"Model not found"}`), "Model not found");
  assert.equal(playgroundErrorText(502, "upstream"), "upstream");
});

// node --test scripts/usage-collector/worker.test.mjs
import assert from "node:assert/strict";
import { test } from "node:test";
import { gzipSync } from "node:zlib";
import worker from "./worker.js";

function bucket() {
  const objects = new Map();
  return { objects, put: async (key, value, opts) => { objects.set(key, { value, opts }); } };
}

const report = { schema: 1, install_id: "0123456789abcdef", day: "2026-10-21", server: { version: "v1" } };
const post = (body, headers = { "Content-Encoding": "gzip", "CF-Connecting-IP": "203.0.113.7" }) => {
  const req = new Request("https://usage.gpuworks.ai/v1/report", { method: "POST", body, headers });
  Object.defineProperty(req, "cf", { value: { country: "CN", region: "Shanghai" } });
  return req;
};

test("a gzip report is kept once per installation and day", async () => {
  const env = { REPORTS: bucket() };
  for (const n of [1, 2]) {
    const res = await worker.fetch(post(gzipSync(JSON.stringify({ ...report, server: { version: `v${n}` } }))), env);
    assert.equal(res.status, 204);
  }
  assert.deepEqual([...env.REPORTS.objects.keys()], ["reports/2026-10-21/0123456789abcdef.json"]);
  const kept = env.REPORTS.objects.get("reports/2026-10-21/0123456789abcdef.json");
  assert.equal(JSON.parse(kept.value).server.version, "v2");
  assert.equal(kept.opts.customMetadata.client_ip, "203.0.113.7");
  assert.equal(kept.opts.customMetadata.country, "CN");
  assert.equal(kept.opts.customMetadata.region, "Shanghai");
});

test("a body already decompressed by the edge is taken as it is", async () => {
  const env = { REPORTS: bucket() };
  assert.equal((await worker.fetch(post(JSON.stringify(report), {}), env)).status, 204);
});

test("anything else is refused and nothing is kept", async () => {
  const env = { REPORTS: bucket() };
  const cases = [
    [new Request("https://usage.gpuworks.ai/v1/report"), 405],
    [new Request("https://usage.gpuworks.ai/other", { method: "POST", body: "x" }), 404],
    [post(gzipSync("not json")), 400],
    [post(gzipSync(JSON.stringify({ ...report, install_id: "../../etc" }))), 400],
    [post(gzipSync(JSON.stringify({ ...report, day: "2026-10-21/../x" }))), 400],
    [post(gzipSync(JSON.stringify({ ...report, schema: "1" }))), 400],
    [post(new Uint8Array([0x1f, 0x8b, 0, 1, 2])), 400],
    [post(new Uint8Array(300 << 10)), 413],
  ];
  for (const [req, status] of cases) {
    assert.equal((await worker.fetch(req, env)).status, status, `${req.method} ${req.url}`);
  }
  assert.equal(env.REPORTS.objects.size, 0);
});

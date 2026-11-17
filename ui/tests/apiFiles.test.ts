// Files to and from the API (ui/src/apiFiles.ts): downloadFile saves a file
// answer under the server's name, saveBlob hands a Blob to the browser, and
// sendForm posts multipart forms; error answers decode into the same localized
// APIError as getJSON and sendJSON. The server is a fetch stub that records
// each request as the browser would send it.
import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, beforeEach, mock, test } from "node:test";
import { dom } from "./helpers/dom.ts";

const { downloadFile, fileNameOf, saveBlob, sendForm } = await import("../src/apiFiles.ts");
const { APIError } = await import("../src/apiError.ts");
// The console's own i18next instance, which api.ts localizes errors with; the
// Node runner has no Vite glob, so its English common bundle is added here.
const i18n = (await import("../src/i18n/index.ts")).default;
const en = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")) as { apiError: Record<string, string> };
i18n.addResourceBundle("en", "common", en);
await i18n.changeLanguage("en");

type Sent = { url: string; method?: string; headers: Record<string, string>; body?: BodyInit | null; credentials?: RequestCredentials };
const originalFetch = globalThis.fetch;
const urlApi = globalThis.URL as unknown as { createObjectURL: (blob: Blob) => string; revokeObjectURL: (url: string) => void };
const originalURL = { create: urlApi.createObjectURL, revoke: urlApi.revokeObjectURL };
const anchorClick = dom.window.HTMLAnchorElement.prototype.click;

let sent: Sent[] = [];
let saved: { name: string; href: string; attached: boolean }[] = [];
let created: Blob[] = [];
let revoked: string[] = [];

function serve(answer: () => Response) {
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    sent.push({ url: String(input), method: init?.method, headers: { ...(init?.headers as Record<string, string> | undefined) }, body: init?.body, credentials: init?.credentials });
    return answer();
  }) as typeof fetch;
}

beforeEach(() => {
  sent = [];
  saved = [];
  created = [];
  revoked = [];
  urlApi.createObjectURL = (blob: Blob) => { created.push(blob); return `blob:test/${created.length}`; };
  urlApi.revokeObjectURL = (url: string) => { revoked.push(url); };
  dom.window.HTMLAnchorElement.prototype.click = function click(this: HTMLAnchorElement) {
    saved.push({ name: this.download, href: this.getAttribute("href") ?? "", attached: this.isConnected });
  };
  mock.timers.enable({ apis: ["setTimeout"] });
});
afterEach(() => {
  mock.timers.reset();
  globalThis.fetch = originalFetch;
  urlApi.createObjectURL = originalURL.create;
  urlApi.revokeObjectURL = originalURL.revoke;
  dom.window.HTMLAnchorElement.prototype.click = anchorClick;
});

const codedError = (status: number, code: string, params: Record<string, unknown>, message: string) => () =>
  new Response(JSON.stringify({ error: message, code, params }), { status, headers: { "Content-Type": "application/json" } });

test("downloadFile saves the answer under the server's file name with the console session", async () => {
  serve(() => new Response("CODE-123\n", { headers: { "Content-Type": "application/octet-stream", "Content-Disposition": "attachment; filename=\"node-request.req\"" } }));
  const name = await downloadFile("/api/v1/x/tools/code?format=req", { fallbackName: "code.txt" });
  assert.equal(name, "node-request.req");
  assert.deepEqual(sent, [{ url: "/api/v1/x/tools/code?format=req", method: "GET", headers: {}, body: undefined, credentials: "include" }]);
  assert.deepEqual(saved, [{ name: "node-request.req", href: "blob:test/1", attached: true }]);
  assert.equal(await created[0].text(), "CODE-123\n");
  assert.equal(document.querySelectorAll("a").length, 0, "the temporary link is removed");
  assert.deepEqual(revoked, [], "the object URL lives while the browser starts reading it");
  mock.timers.tick(1000);
  assert.deepEqual(revoked, ["blob:test/1"]);
});

test("downloadFile falls back to the given name and sends a JSON body when asked", async () => {
  serve(() => new Response("{\"rows\":[]}", { headers: { "Content-Type": "application/json" } }));
  const name = await downloadFile("/api/v1/x/tools/export", { method: "POST", body: { nodes: ["n1", "n2"] }, fallbackName: "export.json" });
  assert.equal(name, "export.json");
  assert.equal(sent[0].method, "POST");
  assert.deepEqual(sent[0].headers, { "Content-Type": "application/json" });
  assert.equal(sent[0].body, "{\"nodes\":[\"n1\",\"n2\"]}");
  assert.deepEqual(saved.map((entry) => entry.name), ["export.json"]);
});

test("downloadFile posts a body given without a method", async () => {
  serve(() => new Response("{}", { headers: { "Content-Type": "application/json" } }));
  await downloadFile("/api/v1/x/tools/export", { body: { nodes: ["n1"] }, fallbackName: "export.json" });
  assert.equal(sent[0].method, "POST", "a GET with a body would make fetch throw");
  assert.equal(sent[0].body, "{\"nodes\":[\"n1\"]}");
  assert.deepEqual(saved.map((entry) => entry.name), ["export.json"]);
});

test("a coded error answer rejects with the localized APIError and saves nothing", async () => {
  serve(codedError(403, "FEATURE_UNAVAILABLE", { feature: "audit" }, "audit is not available"));
  const failure = await downloadFile("/api/v1/x/tools/code", { fallbackName: "code.txt" }).then(() => undefined, (error: unknown) => error);
  assert.ok(failure instanceof APIError);
  assert.equal(failure.code, "FEATURE_UNAVAILABLE");
  assert.equal(failure.status, 403);
  assert.deepEqual(failure.params, { feature: "audit" });
  assert.equal(failure.message, en.apiError.FEATURE_UNAVAILABLE.replace("{{feature}}", "audit"));
  assert.deepEqual(saved, []);
  assert.deepEqual(created, []);
});

test("an aborted download rejects and saves nothing", async () => {
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.signal?.aborted) throw new DOMException("The operation was aborted.", "AbortError");
    return new Response("never");
  }) as typeof fetch;
  const controller = new AbortController();
  controller.abort();
  await assert.rejects(downloadFile("/api/v1/x/tools/code", { fallbackName: "code.txt", signal: controller.signal }), /aborted/);
  assert.deepEqual(saved, []);
});

test("fileNameOf prefers the UTF-8 form, unescapes quotes and keeps only the last path part", () => {
  assert.equal(fileNameOf("attachment; filename=\"plain.req\"; filename*=UTF-8''%E7%94%B3%E8%AF%B7.req"), "申请.req");
  assert.equal(fileNameOf("attachment; filename*=utf-8''report%201.csv"), "report 1.csv");
  assert.equal(fileNameOf("attachment; filename=report.csv"), "report.csv");
  assert.equal(fileNameOf("attachment; filename=\"a \\\"quoted\\\" name.txt\""), "a \"quoted\" name.txt");
  assert.equal(fileNameOf("attachment; filename=\"../../etc/passwd\""), "passwd");
  assert.equal(fileNameOf("attachment; filename=\"C:\\\\temp\\\\x.bin\""), "x.bin");
  assert.equal(fileNameOf("attachment; filename*=UTF-8''%E0%A4%A; filename=\"fallback.bin\""), "fallback.bin", "a malformed escape falls back to the plain name");
  assert.equal(fileNameOf("attachment; filename=\"..\""), undefined);
  assert.equal(fileNameOf("attachment; filename=\"\""), undefined);
  assert.equal(fileNameOf("attachment"), undefined);
  assert.equal(fileNameOf("inline"), undefined);
  assert.equal(fileNameOf(null), undefined);
});

test("saveBlob hands a Blob to the browser under its name", async () => {
  saveBlob(new Blob(["a,b\r\n"], { type: "text/csv" }), "rows.csv");
  assert.deepEqual(saved, [{ name: "rows.csv", href: "blob:test/1", attached: true }]);
  assert.equal(await created[0].text(), "a,b\r\n");
  mock.timers.tick(999);
  assert.deepEqual(revoked, []);
  mock.timers.tick(1);
  assert.deepEqual(revoked, ["blob:test/1"]);
});

test("sendForm posts the form as multipart with the session and reads the JSON answer", async () => {
  serve(() => Response.json({ imported: 1, results: [{ name: "a.bin", outcome: "imported" }] }));
  const form = new FormData();
  form.append("file", new Blob(["payload"]), "a.bin");
  form.append("note", "first");
  const answer = await sendForm<{ imported: number }>("POST", "/api/v1/x/tools/import", form);
  assert.deepEqual(answer, { imported: 1, results: [{ name: "a.bin", outcome: "imported" }] });
  assert.equal(sent.length, 1);
  assert.equal(sent[0].method, "POST");
  assert.equal(sent[0].credentials, "include");
  assert.deepEqual(sent[0].headers, {}, "no Content-Type: the browser writes multipart with its boundary");
  assert.equal(sent[0].body, form);
  // What actually goes over the wire is multipart/form-data with both parts.
  const wire = new Request("http://localhost/", { method: "POST", body: sent[0].body });
  assert.match(wire.headers.get("content-type") ?? "", /^multipart\/form-data; boundary=/);
  const text = await wire.text();
  assert.match(text, /name="file"; filename="a.bin"\r\n[\s\S]*payload/);
  assert.match(text, /name="note"\r\n\r\nfirst/);
});

test("sendForm gives undefined for an empty answer", async () => {
  serve(() => new Response(null, { status: 204 }));
  assert.equal(await sendForm("DELETE", "/api/v1/x/tools/import", new FormData()), undefined);
});

test("sendForm decodes a too-large refusal into its code, parameters and localized text", async () => {
  serve(codedError(413, "BATCH_FILE_TOO_LARGE", { limit_mb: 200 }, "the file is larger than 200 MB"));
  const form = new FormData();
  form.append("file", new Blob(["x"]), "big.jsonl");
  const failure = await sendForm("POST", "/api/v1/x/tools/import", form).then(() => undefined, (error: unknown) => error);
  assert.ok(failure instanceof APIError);
  assert.equal(failure.status, 413);
  assert.equal(failure.code, "BATCH_FILE_TOO_LARGE");
  assert.deepEqual(failure.params, { limit_mb: 200 });
  assert.equal(failure.message, en.apiError.BATCH_FILE_TOO_LARGE.replace("{{limit_mb}}", "200"));
});

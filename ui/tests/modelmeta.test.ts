import assert from "node:assert/strict";
import { test } from "node:test";

import { JSDOM } from "jsdom";
import { fetchHuggingFaceMetadata, usableBases } from "../src/modelmeta.ts";

const bases = async () => ["https://huggingface.co", "https://hf-mirror.com"];

test("Hugging Face metadata falls back to the next address the server gave", async () => {
  const requests: string[] = [];
  const fetcher = (async (input: string | URL | Request) => {
    const url = String(input);
    requests.push(url);
    if (url.startsWith("https://huggingface.co")) throw new Error("official host unavailable");
    if (url.endsWith("/README.md")) return new Response("# Browser README");
    if (url.endsWith("/config.json")) return new Response(`{"architectures":["Qwen3ForCausalLM"]}`);
    if (url.endsWith("/model.safetensors.index.json")) return new Response(`{"metadata":{"total_size":1234}}`);
    if (url.includes("/api/models/")) return new Response(`{"author":"Qwen","siblings":[{"rfilename":"model.safetensors","size":9999}]}`);
    if (url.endsWith("/api/organizations/Qwen/avatar")) return new Response(`{"avatarUrl":"https://cdn.hf-mirror.com/uploads/qwen.png"}`);
    if (url === "https://cdn.hf-mirror.com/uploads/qwen.png") return new Response(new Uint8Array([0x89, 0x50, 0x4e, 0x47]), { headers: { "content-type": "image/png" } });
    return new Response("not found", { status: 404 });
  }) as typeof fetch;

  const metadata = await fetchHuggingFaceMetadata("Qwen/Qwen3-8B", "main", fetcher, bases);
  assert.equal(metadata.readme, "# Browser README");
  assert.match(metadata.config_raw, /Qwen3ForCausalLM/);
  assert.equal(metadata.weight_bytes, 1234);
  assert.equal(metadata.logo_data, "data:image/png;base64,iVBORw==");
  assert.ok(requests.some((url) => url === "https://hf-mirror.com/Qwen/Qwen3-8B/raw/main/README.md"));
  assert.ok(requests.some((url) => url === "https://hf-mirror.com/api/models/Qwen/Qwen3-8B?blobs=true"));
  assert.ok(requests.some((url) => url === "https://hf-mirror.com/api/organizations/Qwen/avatar"));
});

test("Hugging Face metadata sums single-file weights when no index exists", async () => {
  const fetcher = (async (input: string | URL | Request) => {
    const url = String(input);
    if (url.startsWith("https://huggingface.co")) throw new Error("official host unavailable");
    if (url.endsWith("/README.md")) return new Response("# Single-file model");
    if (url.endsWith("/config.json")) return new Response(`{"model_type":"bert"}`);
    if (url.endsWith("/model.safetensors.index.json")) return new Response("not found", { status: 404 });
    if (url.includes("/api/models/")) {
      return new Response(JSON.stringify({
        siblings: [
          { rfilename: "model.safetensors", size: 2048 },
          { rfilename: "pytorch_model.bin", size: 1024 },
          { rfilename: "config.json", size: 100 },
        ],
      }));
    }
    return new Response("not found", { status: 404 });
  }) as typeof fetch;

  const metadata = await fetchHuggingFaceMetadata("acme/single-file-model", "main", fetcher, bases);
  assert.equal(metadata.weight_bytes, 3072);
});

test("a gated repository's README without its config.json is left to the server", async () => {
  const fetcher = (async (input: string | URL | Request) => {
    const url = String(input);
    if (url.endsWith("/README.md")) return new Response("# Gated model card");
    if (url.includes("/api/models/")) return new Response(`{"author":"acme","gated":"manual"}`);
    return new Response("Access to model acme/gated is restricted", { status: 403 });
  }) as typeof fetch;
  await assert.rejects(fetchHuggingFaceMetadata("acme/gated", "main", fetcher, bases), /browser cannot access/);
});

test("Hugging Face metadata fails when every browser route fails", async () => {
  const fetcher = (async () => { throw new Error("offline"); }) as typeof fetch;
  await assert.rejects(fetchHuggingFaceMetadata("Qwen/Qwen3-8B", "main", fetcher, bases), /browser cannot access/);
});

test("the browser asks the server for its addresses and never uses a built-in list", async () => {
  // The console shell wants a DOM only while its modules load.
  Object.defineProperty(globalThis, "document", { value: new JSDOM("<!doctype html>", { url: "http://localhost/" }).window.document, configurable: true });
  const { api } = await import("../src/api.ts");
  delete (globalThis as { document?: unknown }).document;
  const original = api.metadataSources;
  const asked: string[] = [];
  api.metadataSources = (async (source: string) => { asked.push(source); return { source, bases: ["https://mirror.corp.example/"] }; }) as typeof api.metadataSources;
  const requests: string[] = [];
  const fetcher = (async (input: string | URL | Request) => {
    requests.push(String(input));
    return new Response(String(input).endsWith("/config.json") ? "{}" : "x", { status: String(input).includes("/api/models/") ? 404 : 200 });
  }) as typeof fetch;
  try {
    const metadata = await fetchHuggingFaceMetadata("Qwen/Qwen3-8B", "main", fetcher);
    assert.equal(metadata.config_raw, "{}");
  } finally {
    api.metadataSources = original;
  }
  assert.deepEqual(asked, ["huggingface"]);
  assert.ok(requests.length > 0 && requests.every((url) => url.startsWith("https://mirror.corp.example/") || url.startsWith("https://mirror.corp.example")), "only the given address is read");
  assert.ok(requests.every((url) => !url.includes("huggingface.co") && !url.includes("hf-mirror.com")));
  assert.ok(requests.includes("https://mirror.corp.example/Qwen/Qwen3-8B/raw/main/README.md"), "the trailing slash of a base is dropped");
});

test("no allowed address means the browser does not fetch at all", async () => {
  let fetched = 0;
  const fetcher = (async () => { fetched++; return new Response("x"); }) as typeof fetch;
  await assert.rejects(fetchHuggingFaceMetadata("Qwen/Qwen3-8B", "main", fetcher, async () => []), /no Hugging Face metadata source/);
  assert.equal(fetched, 0);
});

test("a server that cannot be asked means the browser does not fetch", async () => {
  let fetched = 0;
  const fetcher = (async () => { fetched++; return new Response("x"); }) as typeof fetch;
  await assert.rejects(fetchHuggingFaceMetadata("Qwen/Qwen3-8B", "main", fetcher, async () => { throw new Error("forbidden"); }), /forbidden/);
  assert.equal(fetched, 0);
});

test("plain http addresses are dropped on an https page, duplicates and junk always", () => {
  const input = ["http://hf.internal:8080", "https://a.example/", "https://a.example", "ftp://x", "  "];
  assert.deepEqual(usableBases(input, "https:"), ["https://a.example"]);
  assert.deepEqual(usableBases(input, "http:"), ["http://hf.internal:8080", "https://a.example"]);
});

test("the commit the repository record reports is returned only when it is a full sha", async () => {
  const sha = "0123456789abcdef0123456789abcdef01234567";
  const fetcherFor = (value: string) => (async (input: string | URL | Request) => {
    const url = String(input);
    if (url.includes("/api/models/")) return new Response(JSON.stringify({ sha: value, siblings: [{ rfilename: "m.safetensors", size: 5 }] }));
    if (url.endsWith("/config.json")) return new Response(`{"model_type":"bert"}`);
    return new Response("nope", { status: 404 });
  }) as typeof fetch;
  assert.equal((await fetchHuggingFaceMetadata("a/b", "main", fetcherFor(sha), bases)).sha, sha);
  assert.equal((await fetchHuggingFaceMetadata("a/b", "main", fetcherFor("main"), bases)).sha, undefined);
});

function avatarFetcher(answers: Record<string, () => Response>, requests: string[] = []) {
  return (async (input: string | URL | Request) => {
    const url = String(input);
    requests.push(url);
    if (url.endsWith("/README.md")) return new Response("# Model");
    if (url.endsWith("/config.json")) return new Response(`{"model_type":"qwen3"}`);
    if (url.includes("/api/models/")) return new Response(`{"author":"someone"}`);
    // The hub's generated gradient answers any name.
    if (url.includes("/avatars/")) return new Response(`<svg width="120" height="120"/>`, { headers: { "content-type": "image/svg+xml" } });
    const answer = answers[url];
    return answer ? answer() : new Response(`{"error":"Sorry, we can't find the page you are looking for."}`, { status: 404 });
  }) as typeof fetch;
}

const hubOnly = async () => ["https://huggingface.co"];
const png = () => new Response(new Uint8Array([1, 2, 3]), { headers: { "content-type": "image/png" } });

test("a Hugging Face logo is the author's uploaded avatar, as an organization or else as a user", async () => {
  const upload = "https://cdn-avatars.huggingface.co/v1/production/uploads/x/someone.png";
  const asOrganization = await fetchHuggingFaceMetadata("someone/model", "main", avatarFetcher({
    "https://huggingface.co/api/organizations/someone/avatar": () => new Response(JSON.stringify({ avatarUrl: upload })),
    [upload]: png,
  }), hubOnly);
  assert.equal(asOrganization.logo_data, "data:image/png;base64,AQID");

  const asUser = await fetchHuggingFaceMetadata("someone/model", "main", avatarFetcher({
    "https://huggingface.co/api/users/someone/avatar": () => new Response(JSON.stringify({ avatarUrl: upload })),
    [upload]: png,
  }), hubOnly);
  assert.equal(asUser.logo_data, "data:image/png;base64,AQID");
});

test("a generated placeholder, a foreign host or a non-image is no Hugging Face logo", async () => {
  for (const [name, avatarUrl, image] of [
    ["placeholder", "https://huggingface.co/avatars/0ad7d56ed9cf77c1049363a953d93883.svg", png],
    ["host the server did not allow", "https://images.example.com/someone.png", png],
    ["html instead of an image", "https://cdn-avatars.huggingface.co/someone.png", () => new Response("<!doctype html>", { headers: { "content-type": "text/html" } })],
    ["missing picture", "https://cdn-avatars.huggingface.co/someone.png", () => new Response("", { status: 404 })],
  ] as const) {
    const requests: string[] = [];
    const metadata = await fetchHuggingFaceMetadata("someone/model", "main", avatarFetcher({
      "https://huggingface.co/api/organizations/someone/avatar": () => new Response(JSON.stringify({ avatarUrl })),
      [avatarUrl]: image,
    }, requests), hubOnly);
    assert.equal(metadata.logo_data, "", name);
    assert.ok(!requests.some((url) => url.includes("/avatars/")), `${name}: the generated /avatars/ gradient is never read`);
  }
  const unknown = await fetchHuggingFaceMetadata("someone/model", "main", avatarFetcher({}), hubOnly);
  assert.equal(unknown.logo_data, "", "an author the hub does not know has no logo");
});

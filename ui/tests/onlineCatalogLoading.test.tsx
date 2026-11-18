import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, beforeEach, test } from "node:test";
import React from "react";
import { JSDOM } from "jsdom";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { createInstance } from "i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", {url: "http://localhost"});
for (const key of ["window", "document", "navigator", "Element", "HTMLElement", "HTMLInputElement", "SVGElement", "Node", "MutationObserver", "ShadowRoot", "Document", "HTMLBodyElement"] as const) {
  Object.defineProperty(globalThis, key, {value: dom.window[key], configurable: true});
}
Object.defineProperty(globalThis, "localStorage", {value: dom.window.localStorage, configurable: true});
Object.defineProperty(globalThis, "getComputedStyle", {value: dom.window.getComputedStyle.bind(dom.window), configurable: true});
Object.defineProperty(dom.window, "matchMedia", {value: () => ({matches: false, addEventListener() {}, removeEventListener() {}})});
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
globalThis.ResizeObserver = ResizeObserverStub;
const observers = new Set<IntersectionObserverCallback>();
class IntersectionObserverStub {
  constructor(private callback: IntersectionObserverCallback) {}
  observe() { observers.add(this.callback); }
  unobserve() {}
  disconnect() { observers.delete(this.callback); }
}
Object.defineProperty(globalThis, "IntersectionObserver", {value: IntersectionObserverStub, configurable: true});
const {act, cleanup, fireEvent, render, renderHook, screen, waitFor} = await import("@testing-library/react");
const {MemoryRouter} = await import("react-router-dom");
const {api} = await import("../src/api.ts");
const {useCatalogPages} = await import("../src/pages/models/discovery/useCatalogPages.ts");
const {readHub} = await import("../src/pages/models/discovery/hubs.ts");
const {default: OnlineCatalog} = await import("../src/pages/models/discovery/OnlineCatalog.tsx");
const models = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/models.json", import.meta.url), "utf8"));
const common = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({lng: "en", resources: {en: {models, common}}, defaultNS: "models", interpolation: {escapeValue: false}});
const original = {fetch: globalThis.fetch, models: api.models, templates: api.inferenceTemplates, sources: api.metadataSources};
const calls: URL[] = [];
let responder: (url: URL, signal?: AbortSignal | null) => Promise<Response>;
const rev = "a".repeat(40);
const model = (id: string) => ({id, sha: rev, tasks: ["text-generation"], likes: 2, downloads: 5});
const page = (ids: string[], total = ids.length) => Response.json({data: {models: ids.map(model), total_count: total}});
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return {resolve, reject, promise};
}
beforeEach(() => {
  calls.length = 0;
  observers.clear();
  localStorage.clear();
  api.models = async () => ({items: []});
  api.inferenceTemplates = async () => ({items: [], total: 0, page: 1, page_size: 50});
  api.metadataSources = async () => ({source: "modelscope", bases: []});
  responder = async () => page(["org/one"]);
  globalThis.fetch = async (input, init) => {
    const url = new URL(String(input), "http://localhost");
    calls.push(url);
    return responder(url, init?.signal);
  };
});
afterEach(() => {
  cleanup();
  globalThis.fetch = original.fetch;
  api.models = original.models;
  api.inferenceTemplates = original.templates;
  api.metadataSources = original.sources;
});
after(() => { closeChannels(); dom.window.close(); });

function hook() {
  return renderHook(({active, query}) => useCatalogPages(active, "modelscope", query, "all", "trending"), {initialProps: {active: true, query: ""}});
}
test("appending preserves previous results, deduplicates overlap and requests one next page", async () => {
  const second = deferred<Response>();
  responder = async url => url.searchParams.get("page_number") === "2" ? second.promise : page(["org/one", "org/two"], 30);
  const h = hook();
  await waitFor(() => assert.equal(h.result.current.result?.items.length, 2));
  act(() => { h.result.current.loadMore(); h.result.current.loadMore(); });
  assert.equal(h.result.current.result?.items.length, 2);
  second.resolve(page(["org/two", "org/three"], 30));
  await waitFor(() => assert.deepEqual(h.result.current.result?.items.map(m => m.id), ["org/one", "org/two", "org/three"]));
  assert.equal(calls.filter(u => u.searchParams.get("page_number") === "2").length, 1);
});
test("a failed next page retains the list and retries that page", async () => {
  let failed = true;
  responder = async url => url.searchParams.get("page_number") === "2"
    ? failed ? new Response("", {status: 503}) : page(["org/next"], 11)
    : page(["org/first"], 11);
  const h = hook();
  await waitFor(() => assert.equal(h.result.current.result?.items.length, 1));
  act(() => h.result.current.loadMore());
  await waitFor(() => assert.ok(h.result.current.error));
  assert.equal(h.result.current.result?.items[0].id, "org/first");
  failed = false;
  act(() => h.result.current.retry());
  await waitFor(() => assert.equal(h.result.current.result?.items.length, 2));
  assert.equal(h.result.current.result?.next, undefined);
});
test("revisiting a query immediately restores it while a failed refresh preserves all loaded pages", async () => {
  responder = async url => url.searchParams.get("search") ? page(["org/search"]) : page(["org/cached"], 30);
  const h = hook();
  await waitFor(() => assert.equal(h.result.current.result?.items[0].id, "org/cached"));
  h.rerender({active: true, query: "new"});
  await waitFor(() => assert.equal(h.result.current.result?.items[0].id, "org/search"));
  const refresh = deferred<Response>();
  responder = async () => refresh.promise;
  h.rerender({active: true, query: ""});
  assert.equal(h.result.current.result?.items[0].id, "org/cached");
  await waitFor(() => assert.equal(calls.length, 3));
  refresh.resolve(new Response("", {status: 504}));
  await waitFor(() => assert.ok(h.result.current.error));
  assert.equal(h.result.current.result?.items[0].id, "org/cached");
});
test("refresh follows the new Hugging Face cursor chain before replacing a cached multi-page result", async () => {
  let refreshing = false;
  responder = async url => {
    const cursor = url.searchParams.get("cursor");
    const prefix = refreshing ? "new" : "old";
    if (cursor) assert.equal(cursor, prefix);
    return Response.json([model("org/" + prefix + (cursor ? "-2" : "-1"))], {headers: cursor ? {} : {"X-Catalog-Link": '<https://huggingface.co/api/models?cursor=' + prefix + '>; rel="next"'}});
  };
  const h = renderHook(({active}) => useCatalogPages(active, "huggingface", "", "all", "trending"), {initialProps: {active: true}});
  await waitFor(() => assert.equal(h.result.current.result?.pages, 1));
  act(() => h.result.current.loadMore());
  await waitFor(() => assert.equal(h.result.current.result?.pages, 2));
  h.rerender({active: false});
  refreshing = true;
  h.rerender({active: true});
  assert.equal(h.result.current.result?.items.length, 2);
  await waitFor(() => assert.deepEqual(h.result.current.result?.items.map(m => m.id), ["org/new-1", "org/new-2"]));
});
test("changing filters aborts pending append and ignores its late result", async () => {
  const next = deferred<Response>();
  let signal: AbortSignal | null | undefined;
  responder = async (url, s) => {
    if (url.searchParams.get("search")) return page(["org/new"]);
    if (url.searchParams.get("page_number") === "2") { signal = s; return next.promise; }
    return page(["org/old"], 30);
  };
  const h = hook();
  await waitFor(() => assert.ok(h.result.current.result));
  act(() => h.result.current.loadMore());
  h.rerender({active: true, query: "new"});
  assert.equal(signal?.aborted, true);
  next.resolve(page(["org/late"], 30));
  await waitFor(() => assert.equal(h.result.current.result?.items[0].id, "org/new"));
  assert.equal(h.result.current.result?.items.length, 1);
});
test("an empty trailing page stops further automatic loading", async () => {
  responder = async url => url.searchParams.get("page_number") === "2" ? page([], 100) : page(["org/first"], 100);
  const h = hook();
  await waitFor(() => assert.ok(h.result.current.result));
  act(() => h.result.current.loadMore());
  await waitFor(() => assert.equal(h.result.current.result?.pages, 2));
  assert.equal(h.result.current.result?.next, undefined);
});
function catalog() {
  return render(<I18nextProvider i18n={i18n}><MemoryRouter><OnlineCatalog active provider="modelscope" onProvider={() => {}} onManual={() => {}} onQuery={() => {}} /></MemoryRouter></I18nextProvider>);
}
test("ModelScope reads README without waiting for revision and does not reload it when template matching resolves", async () => {
  const revision = deferred<Response>();
  const templateVersions: string[] = [];
  api.inferenceTemplates = async params => {
    templateVersions.push(params!.extra!.weight_revision![0]);
    return {items: [], total: 0, page: 1, page_size: 50};
  };
  responder = async url => {
    const path = url.searchParams.get("path") || "";
    if (path.endsWith("/commits")) return revision.promise;
    if (path.endsWith("README.md")) return new Response("# Repository README");
    return Response.json({data: {models: [{id: "org/one", tasks: []}], total_count: 1}});
  };
  catalog();
  await screen.findByRole("button", {name: "org/one"});
  await waitFor(() => assert.ok(calls.some(u => u.searchParams.get("path")?.endsWith("/commits"))));
  await screen.findByRole("heading", {name: "Repository README"});
  const content = screen.getByRole("heading", {name: "Repository README"});
  assert.deepEqual(templateVersions, [], "template matching still requires the exact revision");
  revision.resolve(Response.json({Data: {Commit: [{Id: rev}]}}));
  await waitFor(() => assert.deepEqual(templateVersions, [rev]));
  assert.equal(screen.getByRole("heading", {name: "Repository README"}), content);
  assert.deepEqual(calls.filter(u => u.searchParams.get("path")?.endsWith("README.md")).map(u => u.searchParams.get("path")), ["/models/org/one/resolve/master/README.md"]);
});
test("visible list footer loads more without changing the selected detail or downloading new rows' readmes", async () => {
  responder = async url => {
    const path = url.searchParams.get("path") || "";
    if (path.endsWith("/commits")) return Response.json({Data: {Commit: [{Id: rev}]}});
    if (path.endsWith("README.md")) return new Response("# Selected model");
    return url.searchParams.get("page_number") === "2" ? page(["org/two"], 11) : page(["org/one"], 11);
  };
  catalog();
  await screen.findByRole("heading", {name: "Selected model"});
  await waitFor(() => assert.ok(observers.size));
  act(() => { for (const observer of observers) observer([{isIntersecting: true}] as IntersectionObserverEntry[], {} as IntersectionObserver); });
  await screen.findByRole("button", {name: "org/two"});
  assert.equal(screen.getByRole("button", {name: "org/one"}).getAttribute("aria-pressed"), "true");
  assert.equal(calls.filter(u => u.searchParams.get("path")?.endsWith("README.md")).length, 1);
});
test("the browser fallback wins a stalled response body and cancels the relay", async () => {
  let relaySignal: AbortSignal | null | undefined;
  responder = async (url, signal) => {
    if (url.hostname !== "localhost") return new Response("complete");
    relaySignal = signal;
    return new Response(new ReadableStream({
      start(controller) { signal?.addEventListener("abort", () => controller.error(signal.reason), {once: true}); },
    }));
  };
  api.metadataSources = async () => ({source: "modelscope", bases: ["https://modelscope.cn"]});
  const response = await readHub("modelscope", "https://modelscope.cn/openapi/v1/models", new AbortController().signal);
  assert.equal(await response.text(), "complete");
  assert.equal(relaySignal?.aborted, true);
});
test("rate limits are returned intact when every permitted browser route fails", async () => {
  responder = async () => new Response("limited", {status: 429});
  const response = await readHub("modelscope", "https://modelscope.cn/openapi/v1/models", new AbortController().signal);
  assert.equal(response.status, 429);
});
test("a slow initial response beyond the former 2.8 second cutoff still displays its source", async () => {
  responder = async (url, signal) => {
    if (url.searchParams.get("path")?.endsWith("README.md")) return new Response("# Slow model");
    await new Promise(resolve => setTimeout(resolve, 3000));
    assert.equal(signal?.aborted, false, "the first source must have time to return before fallback");
    return page(["org/slow"]);
  };
  catalog();
  await screen.findByRole("heading", {name: "Slow model"});
  assert.equal(calls.filter(u => u.searchParams.get("source") === "huggingface").length, 0);
});
test("revision failure leaves README readable and retry only refreshes revision and templates", async () => {
  let failed = true;
  responder = async url => {
    const path = url.searchParams.get("path") || "";
    if (path.endsWith("/commits")) return failed ? new Response("", {status: 503}) : Response.json({Data: {Commit: [{Id: rev}]}});
    if (path.endsWith("README.md")) return new Response("# Recovered model");
    return Response.json({data: {models: [{id: "org/one", tasks: []}], total_count: 1}});
  };
  catalog();
  await screen.findByText(models.catalog.revisionError);
  await screen.findByRole("heading", {name: "Recovered model"});
  failed = false;
  fireEvent.click(screen.getByRole("button", {name: models.catalog.retry}));
  await screen.findByRole("heading", {name: "Recovered model"});
  await waitFor(() => assert.equal(screen.queryByText(models.catalog.revisionError), null));
  assert.equal(calls.filter(u => u.searchParams.get("path")?.endsWith("README.md")).length, 1);
});
test("visible registrations retain their state while other models' README and templates stay lazy", async () => {
  api.models = async () => ({items: [{source: "modelscope", source_id: "org/two", revision: rev}]}) as never;
  const templateRepos: string[] = [];
  api.inferenceTemplates = async params => {
    templateRepos.push(params!.extra!.weight_repo![0]);
    return {items: [], total: 0, page: 1, page_size: 50};
  };
  responder = async url => {
    const path = url.searchParams.get("path") || "";
    if (path.endsWith("/commits")) return Response.json({Data: {Commit: [{Id: rev}]}});
    if (path.endsWith("README.md")) return new Response("# " + (path.includes("org/one") ? "First" : "Second"));
    return Response.json({data: {models: [{id: "org/one", tasks: []}, {id: "org/two", tasks: []}], total_count: 2}});
  };
  catalog();
  await screen.findByRole("heading", {name: "First"});
  await waitFor(() => assert.equal(screen.getByRole("button", {name: models.catalog.registered}).hasAttribute("disabled"), true));
  assert.deepEqual(templateRepos, ["org/one"]);
  assert.equal(calls.filter(u => u.searchParams.get("path")?.endsWith("README.md")).length, 1);
  fireEvent.click(screen.getByRole("button", {name: "org/two"}));
  await screen.findByRole("heading", {name: "Second"});
  fireEvent.click(screen.getByRole("button", {name: "org/one"}));
  await screen.findByRole("heading", {name: "First"});
  assert.equal(calls.filter(u => u.searchParams.get("path")?.endsWith("README.md")).length, 2, "returning to an immutable revision reuses its README");
  assert.equal(calls.filter(u => u.searchParams.get("path")?.endsWith("/commits")).length, 2);
});
test("card results do not request details until the user opens a model", async () => {
  const data = deferred<Response>();
  responder = async url => url.searchParams.get("path")?.endsWith("README.md") ? new Response("# Opened card") : data.promise;
  catalog();
  fireEvent.click(screen.getByRole("tab", {name: models.catalog.cards}));
  data.resolve(page(["org/one"]));
  await screen.findByText("org/one");
  assert.equal(calls.filter(u => u.searchParams.get("path")?.endsWith("README.md")).length, 0);
  fireEvent.click(screen.getByRole("button", {name: "org/one"}));
  await screen.findByRole("heading", {name: "Opened card"});
});

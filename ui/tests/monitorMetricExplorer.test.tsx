import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLTextAreaElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const anyContext = new Proxy(function () {} as never, {
  get: (_target: never, prop: string | symbol) => {
    if (prop === "canvas") return { width: 300, height: 220, style: {} };
    if (prop === "dpr") return 1;
    if (prop === "measureText") return () => ({ width: 10 });
    return anyContext;
  },
  set: () => true,
  apply: () => anyContext,
});
dom.window.HTMLCanvasElement.prototype.getContext = () => anyContext as CanvasRenderingContext2D;

const enMonitoring = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/monitoring.json", import.meta.url), "utf8"));
const enCommon = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8"));
const zhCommon = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/zh/common.json", import.meta.url), "utf8"));

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { monitoring: enMonitoring, common: enCommon }, zh: { common: zhCommon } },
  defaultNS: "monitoring",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor, fireEvent } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { apiErrorText, APIError } = await import("../src/apiError.ts");
const { default: MetricExplorer } = await import("../src/pages/monitor/MetricExplorer.tsx");

const originalFetch = globalThis.fetch;
// The recorded GET /api/v1/monitor/metrics response (kept equal to the
// server's by monitorapi's TestMetricVocabularyMatchesTheUIFixture).
const vocabulary = fs.readFileSync(new URL("./fixtures/monitor-metrics.json", import.meta.url), "utf8");

// The real empty GET /api/v1/monitor/series answer.
const EMPTY_SERIES = JSON.stringify({ items: [], total: 0 });

// seriesFetch answers the metric vocabulary request with the recorded
// vocabulary and every series request with seriesBody.
function seriesFetch(seriesBody: string): typeof fetch {
  return (async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    const body = url.includes("/api/v1/monitor/metrics") ? vocabulary : seriesBody;
    return new Response(body, { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
}

afterEach(() => {
  cleanup();
  globalThis.fetch = originalFetch;
});

async function renderExplorer() {
  const result = render(
    <I18nextProvider i18n={i18n}>
      <MetricExplorer range="1h" />
    </I18nextProvider>,
  );
  // The panel is collapsed by default; open it like a real user would.
  await userEvent.setup().click(screen.getByText("Metric explorer"));
  return result;
}

// A long range's series is the monitor page's slowest read: the collapsed
// panel asks for nothing, and opening it asks for the catalog and the series.
test("the collapsed explorer sends no request until it is opened", async () => {
  const requested: string[] = [];
  const answer = seriesFetch(EMPTY_SERIES);
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requested.push(typeof input === "string" ? input : input instanceof URL ? input.href : input.url);
    return answer(input, init);
  }) as typeof fetch;
  render(
    <I18nextProvider i18n={i18n}>
      <MetricExplorer range="168h" />
    </I18nextProvider>,
  );
  await new Promise((resolve) => setTimeout(resolve, 50));
  assert.deepEqual(requested, []);
  await userEvent.setup().click(screen.getByText("Metric explorer"));
  await waitFor(() => assert.ok(requested.some((url) => url.includes("/api/v1/monitor/series?") && url.includes("range=168h"))));
  assert.ok(requested.some((url) => url.includes("/api/v1/monitor/metrics")));
});

// The metric list is long enough that antd's Select virtualizes its
// dropdown (only a windowed subset of options actually exists in the DOM at
// once); typing the exact name into the search box (showSearch,
// optionFilterProp="label") filters the list down to that one match instead
// of depending on where it happens to scroll to.
async function chooseMetric(name: string) {
  const combo = screen.getByRole("combobox", { name: "Metric" });
  fireEvent.mouseDown(combo);
  await userEvent.setup().type(combo, name);
  const option = await screen.findByText(name, { selector: ".ant-select-item-option-content" });
  await userEvent.setup().click(option);
}

// api.ts's own localization goes through the shared production i18n
// singleton (a real i18next instance with no resources in this Node test
// runner - it only loads translations via Vite's import.meta.glob in the
// browser bundle), so a rendered error here shows the raw wire fallback
// text, not the localized sentence - the same is true for every other
// page's api-error test in this suite. What is actually worth verifying is
// (a) that MetricExplorer surfaces error.message on failure at all, the
// same pass-through every other page uses (DataTable, MonitorPage,
// ServiceMonitorPage), and (b) that the new locale entries themselves
// produce the right sentence when actually looked up through i18next.
test("MetricExplorer shows the api error's message on failure, the same way other pages do", async () => {
  globalThis.fetch = (async () =>
    new Response(JSON.stringify({ error: "series too large", code: "MONITOR_SERIES_TOO_LARGE" }), {
      status: 413,
      headers: { "Content-Type": "application/json" },
    })) as typeof fetch;

  await renderExplorer();
  await waitFor(() => assert.ok(screen.getByText("series too large")));
});

test("MetricExplorer shows nothing extra when the series request succeeds", async () => {
  globalThis.fetch = seriesFetch(EMPTY_SERIES);
  await renderExplorer();
  await waitFor(() => assert.ok(screen.getByText(/Request:/)));
  assert.equal(screen.queryByText(/too large/), null);
});

for (const [language, common] of [["en", enCommon], ["zh", zhCommon]] as const) {
  test(`apiError.MONITOR_SERIES_TOO_LARGE resolves to the real ${language} locale sentence through i18next`, () => {
    // getFixedT("common") mirrors src/i18n/index.ts's defaultNS: "common" -
    // the same binding api.ts's localizedResponseError uses against the
    // production singleton, just against a fixture-loaded instance instead
    // of the empty one Node tests get (import.meta.glob only runs in Vite).
    const t = i18n.getFixedT(language, "common");
    const error = new APIError("MONITOR_SERIES_TOO_LARGE", undefined, "series too large", 413);
    const text = apiErrorText((key, options) => t(key, options), error);
    assert.equal(text, common.apiError.MONITOR_SERIES_TOO_LARGE);
  });
}

test("the metric list includes the phase-2 server-emitted names: engine histograms, preemptions, proxy, GPU bandwidth/clock/throttle, node cpu/disk/net", async () => {
  globalThis.fetch = seriesFetch(EMPTY_SERIES);
  await renderExplorer();
  const combo = screen.getByRole("combobox", { name: "Metric" });
  const user = userEvent.setup();
  for (const name of [
    "gpuworks_infer_ttft_seconds",
    "gpuworks_infer_tpot_seconds",
    "gpuworks_infer_e2e_seconds",
    "gpuworks_infer_queue_time_seconds",
    "gpuworks_infer_prefill_time_seconds",
    "gpuworks_infer_decode_time_seconds",
    "gpuworks_infer_preemptions_total",
    "gpuworks_infer_requests_success_total",
    "gpuworks_proxy_requests_total",
    "gpuworks_proxy_requests_inflight",
    "gpuworks_proxy_request_duration_ms_sum",
    "gpuworks_proxy_request_duration_ms_count",
    "gpuworks_proxy_accounting_dropped_total",
    "gpuworks_gpu_memory_bandwidth_utilization",
    "gpuworks_gpu_sm_clock_mhz",
    "gpuworks_gpu_sm_clock_max_mhz",
    "gpuworks_gpu_sm_active",
    "gpuworks_gpu_sm_occupancy",
    "gpuworks_gpu_tensor_active",
    "gpuworks_gpu_throttle_reasons",
    "gpuworks_node_cpu_busy_percent",
    "gpuworks_node_disk_read_bytes_per_sec",
    "gpuworks_node_disk_write_bytes_per_sec",
    "gpuworks_node_net_rx_bytes_per_sec",
    "gpuworks_node_net_tx_bytes_per_sec",
  ]) {
    fireEvent.mouseDown(combo);
    // The list is long enough to virtualize; filter down to the exact
    // match instead of depending on scroll position.
    await user.clear(combo);
    await user.type(combo, name);
    assert.ok(await screen.findByText(name, { selector: ".ant-select-item-option-content" }), `missing metric option ${name}`);
  }
});

test("selecting a quantile-family histogram metric shows the per-quantile filtering caveat", async () => {
  globalThis.fetch = seriesFetch(EMPTY_SERIES);
  await renderExplorer();
  assert.equal(screen.queryByText(/quantile/i), null, "the default (non-histogram) metric shows no caveat");
  await chooseMetric("gpuworks_infer_ttft_seconds");
  await waitFor(() => assert.ok(screen.getByText(/"quantile" label/)));
});

test("selecting a non-histogram metric after a histogram one hides the caveat again", async () => {
  globalThis.fetch = seriesFetch(EMPTY_SERIES);
  await renderExplorer();
  await chooseMetric("gpuworks_infer_ttft_seconds");
  await waitFor(() => assert.ok(screen.getByText(/"quantile" label/)));
  await chooseMetric("gpuworks_node_cpu_busy_percent");
  await waitFor(() => assert.equal(screen.queryByText(/"quantile" label/), null));
});

test("selecting a quantile-family histogram metric defaults the aggregation to max, not avg", async () => {
  globalThis.fetch = seriesFetch(EMPTY_SERIES);
  await renderExplorer();
  const fnValueText = () =>
    screen.getByRole("combobox", { name: "Aggregation" }).closest(".ant-select")?.querySelector(".ant-select-selection-item")?.textContent;
  assert.equal(fnValueText(), "avg", "the initial (non-histogram) metric keeps the plain avg default");

  await chooseMetric("gpuworks_infer_ttft_seconds");
  await waitFor(() => assert.equal(fnValueText(), "max"));

  // Switching to another histogram family keeps re-defaulting to max even
  // after a user could have changed it away.
  await chooseMetric("gpuworks_infer_decode_time_seconds");
  await waitFor(() => assert.equal(fnValueText(), "max"));
});

// The inference metrics come from the server's vocabulary: the gateway's
// first-token histogram is listed and is a quantile family like an engine's.
test("the gateway's inference series come from the vocabulary, histograms as quantile families", async () => {
  globalThis.fetch = seriesFetch(EMPTY_SERIES);
  await renderExplorer();
  await chooseMetric("gpuworks_proxy_ttft_ms");
  await waitFor(() => assert.ok(screen.getByText(/"quantile" label/)));
  await chooseMetric("gpuworks_proxy_completion_tokens_total");
  await waitFor(() => assert.equal(screen.queryByText(/"quantile" label/), null));
});

// The explorer spells no metric names: every group and option comes from the
// server's catalog, grouped by what measures the series, including the
// platform's own network, KV-disk and benchmark series.
test("metric groups and options come from the server's catalog", async () => {
  globalThis.fetch = seriesFetch(EMPTY_SERIES);
  await renderExplorer();
  const combo = screen.getByRole("combobox", { name: "Metric" });
  const user = userEvent.setup();
  // The list virtualizes; filter to one series at a time and check the
  // group it is listed under.
  for (const [series, group] of [
    ["gpuworks_gpu_utilization", "GPU"],
    ["gpuworks_dgpu_sm_util", "dGPU"],
    ["gpuworks_node_mem_used_bytes", "Node"],
    ["gpuworks_node_rdma_events_per_sec", "Network"],
    ["gpuworks_infer_requests_running", "Inference engine"],
    ["gpuworks_proxy_routed_total", "Gateway"],
    ["gpuworks_kv_disk_used_bytes", "KV cache disk"],
    ["gpuworks_bench_output_tput", "Benchmark"],
  ]) {
    fireEvent.mouseDown(combo);
    await user.clear(combo);
    await user.type(combo, series);
    assert.ok(await screen.findByText(series, { selector: ".ant-select-item-option-content" }), `missing ${series}`);
    assert.ok(screen.getByText(group, { selector: ".ant-select-item-group" }), `${series} is not under ${group}`);
  }
  await user.clear(combo);
  await chooseMetric("gpuworks_node_rdma_events_per_sec");
  await waitFor(() => assert.ok(screen.getByText(/metric=gpuworks_node_rdma_events_per_sec/)));
});

test("an empty catalog leaves the explorer without metric options rather than inventing names", async () => {
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    const body = url.includes("/api/v1/monitor/metrics") ? JSON.stringify({ items: [], total: 0 }) : EMPTY_SERIES;
    return new Response(body, { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  await renderExplorer();
  fireEvent.mouseDown(screen.getByRole("combobox", { name: "Metric" }));
  await waitFor(() => assert.equal(document.querySelectorAll(".ant-select-item-option").length, 0));
});

test("explorer series name their node or deployment, a deleted one marked deleted", async () => {
  const { explorerSeriesName } = await import("../src/pages/monitor/monitorSeries.ts");
  const t = i18n.t.bind(i18n);
  assert.equal(explorerSeriesName(t, { node: "n-1", node_name: "gpu3" }, 0), "gpu3");
  assert.equal(explorerSeriesName(t, { node: "n-2", node_name: "gpu4", node_deleted: "true" }, 0), "gpu4 (deleted)");
  assert.equal(explorerSeriesName(t, { node: "n-2", node_name: "gpu4", node_deleted: "true", gpu: "1" }, 0), "gpu4 (deleted) · GPU 1");
  assert.equal(explorerSeriesName(t, { deployment: "7", deployment_name: "qwen" }, 0), "qwen");
  assert.equal(explorerSeriesName(t, { deployment: "8", deployment_name: "old", deployment_deleted: "true" }, 0), "old (deleted)");
  assert.equal(explorerSeriesName(t, { deployment: "9" }, 0), "#9");
});

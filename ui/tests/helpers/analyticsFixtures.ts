// Fixtures for the operations analytics tests: JSON shaped exactly like
// internal/server/api/dto/analytics.go, for a fixed "now" in the statistics
// zone Asia/Shanghai (a zone the test machine is unlikely to be in, so a page
// reading the browser's zone fails loudly).
import type {
  AnalyticsRange, GPUAnalytics, GPUCardRow, GPUTimeAnalytics, KeyUsageList, QualityAnalytics, QualityFigures, UsageAnalytics, UsageCounts, UsageRow,
} from "../../src/analyticsApi.ts";
import type { NodeFacets } from "../../src/api.ts";

export const ZONE = "Asia/Shanghai";
/** 2026-10-22 14:30 in Asia/Shanghai. */
export const NOW = Date.UTC(2026, 9, 22, 6, 30);
/** 2026-10-22 00:00 in Asia/Shanghai. */
export const TODAY_START = Date.UTC(2026, 9, 21, 16, 0);
export const HOUR = 3_600_000;
export const DAY = 24 * HOUR;

/** The anyContext trick the monitor tests use: ECharts needs a 2d context that jsdom cannot provide. */
export function installCanvas(dom: { window: { HTMLCanvasElement: { prototype: { getContext: unknown } } } }) {
  const anyContext: unknown = new Proxy(function () {} as never, {
    get: (_target: never, prop: string | symbol) => {
      if (prop === "canvas") return { width: 300, height: 200, style: {} };
      if (prop === "dpr") return 1;
      if (prop === "measureText") return () => ({ width: 10 });
      return anyContext;
    },
    set: () => true,
    apply: () => anyContext,
  });
  dom.window.HTMLCanvasElement.prototype.getContext = () => anyContext;
}

export function retentionView(over: Record<string, unknown> = {}) {
  return {
    value: {
      metrics_hours: 72, logs_hours: 72, request_log_days: 7, request_log_max_mb: 2048,
      usage_hourly_days: 31, usage_daily_days: 400, stats_hourly_days: 31, stats_daily_days: 400,
      quality: true, gpu: true, gpu_time: true, time_zone: ZONE, ...over,
    },
    limits: {},
    server_time_zone: "UTC",
  };
}

export const deployments = {
  items: [
    { id: 1, name: "glm-5-3-flash-nvfp4", served_name: "glm-5.3-flash" },
    { id: 3, name: "qwen3-32b", served_name: "qwen3-32b" },
  ],
  total: 2,
};

export function counts(over: Partial<UsageCounts> = {}): UsageCounts {
  return {
    requests: 0, no_usage: 0, prompt_tokens: 0, completion_tokens: 0, reasoning_tokens: 0, reasoning_reported: 0,
    cached_tokens: 0, cache_reported: 0, cache_reported_prompt: 0, ...over,
  };
}

/** n hourly buckets from `from`, or n daily ones in the zone (a day is 24 h here: Shanghai has no DST). */
export function range(from: number, n: number, granularity: "hour" | "day", over: Partial<AnalyticsRange> = {}): AnalyticsRange {
  const step = granularity === "hour" ? HOUR : DAY;
  return {
    from_ms: from, to_ms: from + n * step, granularity, time_zone: ZONE, earliest_ms: from, live_since_ms: 0,
    buckets: Array.from({ length: n }, (_, i) => from + i * step), ...over,
  };
}

function usageRow(over: Partial<UsageRow>): UsageRow {
  return {
    api_key_id: 11, api_key_name: "111", api_key_prefix: "gw-5fd30737e", api_key_revoked: false, api_key_deleted: false,
    user_id: 1, user_name: "admin", deployment_id: 1, deployment_name: "glm-5-3-flash-nvfp4", served_name: "glm-5.3-flash", deployment_deleted: false,
    source: "api", first_ms: TODAY_START + HOUR, last_ms: TODAY_START + 13 * HOUR, ...counts(), ...over,
  };
}

export const KEY_111 = { requests: 1000, no_usage: 10, prompt_tokens: 9_000_000, cached_tokens: 7_200_000, cache_reported: 990, cache_reported_prompt: 9_000_000, completion_tokens: 300_000, reasoning_tokens: 120_000, reasoning_reported: 990 };
export const KEY_MTG = { requests: 200, no_usage: 20, prompt_tokens: 1_000_000, cached_tokens: 100_000, cache_reported: 100, cache_reported_prompt: 500_000, completion_tokens: 50_000, reasoning_tokens: 0, reasoning_reported: 0 };

/** Today so far, hour by hour (15 buckets), the first two filled in from the request log. */
export function usageToday(): UsageAnalytics {
  const r = range(TODAY_START, 15, "hour", { live_since_ms: TODAY_START + 2 * HOUR });
  const rows: UsageRow[] = [
    usageRow({ ...KEY_111 }),
    usageRow({ ...KEY_111, requests: 100, no_usage: 0, prompt_tokens: 800_000, cached_tokens: 0, cache_reported: 0, cache_reported_prompt: 0, completion_tokens: 20_000, reasoning_tokens: 0, reasoning_reported: 0, deployment_id: 3, deployment_name: "qwen3-32b", served_name: "qwen3-32b" }),
    usageRow({ api_key_id: 12, api_key_name: "MarketToGetherGroup", api_key_prefix: "gw-6455c72ca", ...KEY_MTG }),
    usageRow({ api_key_id: 13, api_key_name: "trial-2026q3", api_key_prefix: "gw-1a2b3c4d5", api_key_revoked: true, requests: 4, prompt_tokens: 40_000, cached_tokens: 10_000, cache_reported: 4, cache_reported_prompt: 40_000, completion_tokens: 2_000 }),
    usageRow({ api_key_id: 0, api_key_name: "", api_key_prefix: "", user_id: 1, user_name: "admin", source: "playground", requests: 8, prompt_tokens: 20_000, cached_tokens: 5_000, cache_reported: 8, cache_reported_prompt: 20_000, completion_tokens: 3_000, reasoning_tokens: 2_000, reasoning_reported: 8 }),
    usageRow({ api_key_id: 7, api_key_name: "", api_key_prefix: "", api_key_deleted: true, requests: 3, prompt_tokens: 3_000, completion_tokens: 300 }),
    usageRow({ api_key_id: 12, api_key_name: "MarketToGetherGroup", api_key_prefix: "gw-6455c72ca", deployment_id: 9, deployment_name: "", served_name: "old-model", deployment_deleted: true, requests: 2, prompt_tokens: 2_000, completion_tokens: 200 }),
  ];
  const totals = counts();
  for (const row of rows) {
    for (const f of Object.keys(totals) as (keyof UsageCounts)[]) totals[f] += row[f];
  }
  const series = r.buckets.map((_, i) => counts({
    requests: 10 + i, no_usage: i % 3 === 0 ? 1 : 0, prompt_tokens: 1000 * (i + 1), cached_tokens: 700 * (i + 1), cache_reported: 10 + i, cache_reported_prompt: 1000 * (i + 1),
    completion_tokens: 100 * (i + 1), reasoning_tokens: 40 * (i + 1), reasoning_reported: 10 + i,
  }));
  return { range: r, totals, series, rows };
}

export function qualityFigures(over: Partial<QualityFigures> = {}): QualityFigures {
  return {
    requests: 0, completed: 0, length: 0, client_closed: 0, interrupted: 0, error: 0,
    ttft_p50_ms: null, ttft_p95_ms: null, e2e_p50_ms: null, e2e_p95_ms: null, output_tokens_per_sec: null, ...over,
  };
}

export function qualityToday(): QualityAnalytics {
  const r = range(TODAY_START, 15, "hour");
  const totals = qualityFigures({
    requests: 1000, completed: 940, length: 20, client_closed: 15, interrupted: 5, error: 20,
    ttft_p50_ms: 550, ttft_p95_ms: 1820, e2e_p50_ms: 5200, e2e_p95_ms: 18750, output_tokens_per_sec: 56.25,
  });
  return {
    range: r,
    totals,
    series: r.buckets.map((_, i) => (i === 3
      ? qualityFigures()
      : qualityFigures({ requests: 60, completed: 56, length: 1, client_closed: 1, interrupted: 0, error: 2, ttft_p50_ms: 500 + i * 10, ttft_p95_ms: 1500 + i * 20, e2e_p50_ms: 5000, e2e_p95_ms: 17000 }))),
    rows: [
      { deployment_id: 1, deployment_name: "glm-5-3-flash-nvfp4", served_name: "glm-5.3-flash", deployment_deleted: false, ...totals },
      { deployment_id: 3, deployment_name: "qwen3-32b", served_name: "qwen3-32b", deployment_deleted: false, ...qualityFigures({ requests: 0 }) },
    ],
  };
}

function card(over: Partial<GPUCardRow>): GPUCardRow {
  return {
    node_uuid: "node-a", node_name: "6000*4", node_deleted: false, gpu_index: 0, gpu_uuid: "GPU-a0", gpu_model: "RTX 6000D", deployments: ["glm-5-3-flash-nvfp4"], present: true,
    util_avg: 62.5, util_max: 97, mem_used_avg_bytes: 70 * 1024 ** 3, mem_used_max_bytes: 76 * 1024 ** 3, mem_total_bytes: 84 * 1024 ** 3,
    power_avg_watts: 220, temp_avg_celsius: 61, temp_max_celsius: 70, util_samples: 5000, power_samples: 5000, ...over,
  };
}

export function gpusToday(over: Partial<GPUAnalytics> = {}): GPUAnalytics {
  const r = range(TODAY_START, 4, "hour");
  const cards = [
    card({}),
    card({ gpu_index: 1, gpu_uuid: "GPU-a1", util_avg: 3.2, util_max: 11, power_avg_watts: 90, deployments: [] }),
    card({ node_uuid: "node-b", node_name: "PPU810E*16", gpu_index: 0, gpu_uuid: "GPU-b0", gpu_model: "PPU 810E", util_avg: 1.5, util_max: 9, mem_used_avg_bytes: 72 * 1024 ** 3, mem_total_bytes: 96 * 1024 ** 3, mem_used_max_bytes: 73 * 1024 ** 3, power_avg_watts: 71, temp_max_celsius: 48, deployments: ["glm53-flash-w8a8"] }),
    card({ node_uuid: "node-c", node_name: "gone-node", node_deleted: true, gpu_index: 0, gpu_uuid: "GPU-c0", present: false, util_avg: null, util_max: null, mem_used_avg_bytes: null, mem_used_max_bytes: null, power_avg_watts: null, temp_avg_celsius: null, temp_max_celsius: null, util_samples: 0, power_samples: 0, deployments: [] }),
  ];
  return {
    range: r,
    metric: "util",
    group_by: "model",
    totals: {
      util_avg: 22.4, util_max: 97, mem_used_avg_bytes: 70 * 1024 ** 3, mem_used_max_bytes: 76 * 1024 ** 3, mem_total_bytes: 96 * 1024 ** 3,
      power_avg_watts: 160, temp_avg_celsius: 55, temp_max_celsius: 70, util_samples: 15000, power_samples: 15000,
    },
    card_count: 3,
    by_model: [{ model: "PPU 810E", cards: 1 }, { model: "RTX 6000D", cards: 2 }],
    detail: true,
    cards,
    groups: [],
    heat: [
      { key: "PPU 810E", label: "PPU 810E", node_deleted: false, values: [1.2, 1.5, null, 2.0] },
      { key: "RTX 6000D", label: "RTX 6000D", node_deleted: false, values: [10, 40.5, 80, null] },
    ],
    heat_total: 2,
    ...over,
  };
}

/** gpusFleet is a fleet past the cards answered one by one: 118 H100 nodes and 2 L40S nodes of 8 cards, by model. */
export function gpusFleet(over: Partial<GPUAnalytics> = {}): GPUAnalytics {
  const figures = (util: number, memTotal: number, power: number) => ({
    util_avg: util, util_max: 100, mem_used_avg_bytes: memTotal / 2 * (memTotal === 80e9 ? 1 : 0.5), mem_used_max_bytes: memTotal, mem_total_bytes: memTotal,
    power_avg_watts: power, temp_avg_celsius: 60, temp_max_celsius: 80, util_samples: 1000, power_samples: 1000,
  });
  return {
    range: range(TODAY_START, 4, "hour"),
    metric: "util",
    group_by: "model",
    totals: { ...figures(57.9, 80e9, 298), mem_used_avg_bytes: 39e9 },
    card_count: 960,
    by_model: [{ model: "H100", cards: 944 }, { model: "L40S", cards: 16 }],
    detail: false,
    cards: [],
    groups: [
      { key: "H100", label: "H100", node_deleted: false, cards: 944, nodes: 118, models: ["H100"], deployments: [{ name: "glm-5-3-flash-nvfp4", cards: 4 }], unassigned: 940, ...figures(58.5, 80e9, 300) },
      { key: "L40S", label: "L40S", node_deleted: false, cards: 16, nodes: 2, models: ["L40S"], deployments: [], unassigned: 16, ...figures(20, 48e9, 200) },
    ],
    heat: [
      { key: "H100", label: "H100", node_deleted: false, values: [50, 60, 62, 61] },
      { key: "L40S", label: "L40S", node_deleted: false, values: [20, 20, 20, 20] },
    ],
    heat_total: 2,
    ...over,
  };
}

export function gpuTimeToday(): GPUTimeAnalytics {
  const r = range(TODAY_START, 4, "hour");
  return {
    range: r,
    rows: [
      { deployment_id: 1, deployment_name: "glm-5-3-flash-nvfp4", served_name: "glm-5.3-flash", deployment_deleted: false, gpu_model: "RTX 6000D", gpu_hours: 56, whole_gpu_hours: 56, avg_instances: 1, max_instances: 1, prompt_tokens: 9_800_000, completion_tokens: 320_000, gpu_hours_per_million_output: 175, gpu_hours_per_million_tokens: 5.53 },
      { deployment_id: 1, deployment_name: "glm-5-3-flash-nvfp4", served_name: "glm-5.3-flash", deployment_deleted: false, gpu_model: "PPU 810E", gpu_hours: 24, whole_gpu_hours: 30, avg_instances: 0.5, max_instances: 1, prompt_tokens: 9_800_000, completion_tokens: 320_000, gpu_hours_per_million_output: 175, gpu_hours_per_million_tokens: 5.53 },
      { deployment_id: 3, deployment_name: "qwen3-32b", served_name: "qwen3-32b", deployment_deleted: false, gpu_model: "RTX 6000D", gpu_hours: 8, whole_gpu_hours: 8, avg_instances: 1, max_instances: 1, prompt_tokens: 0, completion_tokens: 0, gpu_hours_per_million_output: null, gpu_hours_per_million_tokens: null },
    ],
    series: [
      { deployment_id: 1, deployment_name: "glm-5-3-flash-nvfp4", gpu_hours: [20, 20, 20, 20] },
      { deployment_id: 3, deployment_name: "qwen3-32b", gpu_hours: [2, 2, 2, 2] },
    ],
    capacity: [
      { gpu_model: "RTX 6000D", total_gpu_hours: 100, schedulable_gpu_hours: 80, allocated_gpu_hours: 64 },
      { gpu_model: "PPU 810E", total_gpu_hours: 100, schedulable_gpu_hours: 20, allocated_gpu_hours: 16 },
    ],
  };
}

export const keyUsage: KeyUsageList = {
  time_zone: ZONE,
  items: [
    { prefix: "gw-live00000", today: counts({ requests: 20, prompt_tokens: 1_200_000, cached_tokens: 900_000, completion_tokens: 30_000 }), last_7_days: counts({ requests: 150, prompt_tokens: 9_000_000, cached_tokens: 6_000_000, completion_tokens: 250_000 }) },
    { prefix: "gw-idle00000", today: counts(), last_7_days: counts({ requests: 3, prompt_tokens: 3000, completion_tokens: 500 }) },
  ],
};

/** nodeFacets is GET /nodes/facets for the fleet: a written pool label and the card model system label. */
export const nodeFacets: NodeFacets = {
  total: 3,
  labels: [{ key: "pool", values: [{ value: "inference", nodes: 2 }, { value: "training", nodes: 1 }] }],
  system_labels: [{ key: "gpuworks.ai/gpu-model", values: [{ value: "rtx-6000d", display: "RTX 6000D", nodes: 2 }] }],
  gpus: [],
};

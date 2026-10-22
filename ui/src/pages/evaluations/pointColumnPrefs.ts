import type { BenchmarkPoint } from "../../api";
import { interactivity, pointTimingValue } from "./evaluationUtils";

export const POINT_COLUMNS_STORAGE_KEY = "gpuworks.evaluations.point-columns.v1";

export const POINT_COLUMN_KEYS = [
	"point", "concurrency", "request_rate", "length", "avg_output", "avg_total", "request", "interactivity",
	"ttft_p99", "tpot_p99", "latency_p99", "sla", "mark", "detail", "max_output", "max_total", "avg_ttft", "avg_tpot",
	"avg_latency", "success_rate", "ttft_p50", "tpot_p50", "latency_p50", "duration", "request_total",
] as const;

export type PointColumnKey = typeof POINT_COLUMN_KEYS[number];
export type PointColumnPreference = { order: PointColumnKey[]; visible: PointColumnKey[] };

export const DEFAULT_VISIBLE_POINT_COLUMNS: PointColumnKey[] = [
	"point", "concurrency", "request_rate", "length", "avg_output", "avg_total", "request", "interactivity",
	"ttft_p99", "tpot_p99", "latency_p99", "sla", "mark", "detail",
];

// Columns that a saved preference may still name. The per-user output column
// became interactivity; the per-user total column was removed.
const RENAMED_COLUMNS: Record<string, PointColumnKey | null> = { per_user_output: "interactivity", per_user_total: null };

export function defaultPointColumnPreference(): PointColumnPreference {
	return { order: [...POINT_COLUMN_KEYS], visible: [...DEFAULT_VISIBLE_POINT_COLUMNS] };
}

// parsePointColumnPreference restores a saved column layout. A layout saved
// by another version keeps the user's order and visibility for the columns it
// knows: renamed columns follow their new key, removed ones are dropped, and
// columns it has never seen are inserted after their default predecessor with
// their default visibility. Corrupt layouts fall back to the default.
export function parsePointColumnPreference(raw: string | null | undefined): PointColumnPreference {
	if (!raw) return defaultPointColumnPreference();
	try {
		const parsed = JSON.parse(raw) as Partial<Record<"order" | "visible", unknown>>;
		if (!Array.isArray(parsed.order) || !Array.isArray(parsed.visible)) return defaultPointColumnPreference();
		const known = new Set<string>(POINT_COLUMN_KEYS);
		const migrate = (keys: unknown[]) => keys.map((key) => typeof key === "string" && key in RENAMED_COLUMNS ? RENAMED_COLUMNS[key] : key).filter((key): key is PointColumnKey => typeof key === "string" && known.has(key));
		const order = migrate(parsed.order), visible = migrate(parsed.visible);
		if (new Set(order).size !== order.length || new Set(visible).size !== visible.length || !visible.includes("point") || order[0] !== "point") return defaultPointColumnPreference();
		const savedOrder = new Set(order);
		POINT_COLUMN_KEYS.forEach((key, index) => {
			if (savedOrder.has(key)) return;
			const previous = POINT_COLUMN_KEYS[index - 1];
			order.splice(order.indexOf(previous) + 1, 0, key);
			if (DEFAULT_VISIBLE_POINT_COLUMNS.includes(key)) visible.push(key);
		});
		return { order, visible: visible.filter((key) => savedOrder.has(key) || DEFAULT_VISIBLE_POINT_COLUMNS.includes(key)) };
	} catch {
		return defaultPointColumnPreference();
	}
}

export function pointDerivedValues(point: BenchmarkPoint) {
	const totalRequests = point.success + point.errors;
	const started = point.started_at ? Date.parse(point.started_at) : Number.NaN;
	const ended = point.ended_at ? Date.parse(point.ended_at) : Number.NaN;
	return {
		interactivity: interactivity(pointTimingValue(point, "p50_tpot")),
		successRate: totalRequests > 0 ? point.success / totalRequests * 100 : undefined,
		requestTotal: totalRequests > 0 ? totalRequests : undefined,
		durationSeconds: Number.isFinite(started) && Number.isFinite(ended) && ended >= started ? (ended - started) / 1000 : undefined,
		maxOutput: point.max_output_tput > 0 ? point.max_output_tput : undefined,
		maxTotal: point.max_total_tput > 0 ? point.max_total_tput : undefined,
	};
}

import type { BenchmarkPoint } from "../../api";
import type { TFunction } from "i18next";
import { format, interactivity, pointParameterLabel, pointTimingValue, pointTokenRate } from "./evaluationUtils.ts";

export type ParetoDatum = { seq: number; x: number; y: number; point: BenchmarkPoint };

// paretoData plots each point's system output throughput against its
// interactivity (1000 / TPOT P50). Points without a measured TPOT have no
// interactivity and are left out rather than drawn at zero.
export function paretoData(points: BenchmarkPoint[]): ParetoDatum[] {
	return points.flatMap((point) => {
		const x = interactivity(pointTimingValue(point, "p50_tpot"));
		const y = pointTokenRate(point, "output_tput");
		return x == null || y == null ? [] : [{ seq: point.seq, x, y, point }];
	});
}

function tr(t: TFunction | undefined, key: string, fallback: string, params: Record<string, unknown> = {}) {
	if (t) return String(t(key, params));
	return fallback.replace(/{{(\w+)}}/g, (_match, name: string) => String(params[name] ?? ""));
}

export function paretoTooltip(datum: ParetoDatum, t?: TFunction) {
	const point = datum.point;
	const total = point.success + point.errors;
	const successRate = total ? point.success / total * 100 : 0;
	return [
		`<b>${tr(t, "pareto.tooltip.point", "Point #{{value}}", { value: point.seq })}</b> · ${pointParameterLabel(point)}`,
		`${tr(t, "pareto.tooltip.interactivity", "Interactivity")}: ${format(datum.x)} tok/s/user`,
		`${tr(t, "pareto.tooltip.average", "Average output throughput")}: ${format(datum.y)} tok/s`,
		`${tr(t, "pareto.tooltip.total", "Average total throughput")}: ${format(pointTokenRate(point, "total_tput"))} tok/s`,
		`${tr(t, "pareto.tooltip.request", "Request throughput")}: ${format(point.request_tput)} req/s`,
		`TTFT P99: ${format(pointTimingValue(point, "p99_ttft"))} ms`,
		`TPOT P50: ${format(pointTimingValue(point, "p50_tpot"))} ms`,
		`${tr(t, "pareto.tooltip.latency", "E2E latency P99")}: ${format(pointTimingValue(point, "p99_latency"))} ms`,
		`${tr(t, "pareto.tooltip.success", "Success rate")}: ${format(successRate)}%`,
	].join("<br/>");
}

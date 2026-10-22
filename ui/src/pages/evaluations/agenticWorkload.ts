import type { BenchmarkMeta, BenchmarkRun } from "../../api";


export function agenticDesignPrefixReuseRate(dataset?: BenchmarkRun["dataset"]) {
	const explicit = dataset?.design_prefix_reuse_rate;
	if (explicit != null && Number.isFinite(explicit)) return explicit > 1 ? explicit / 100 : explicit;
	return undefined;
}

export function agenticSessionsPerPoint(meta: BenchmarkMeta, concurrencies = meta.agentic_defaults.concurrency) {
	return Object.fromEntries(concurrencies.map((concurrency) => [String(concurrency),
		meta.agentic_defaults.sessions_per_point[String(concurrency)]
		|| Math.max(meta.agentic_defaults.min_sessions_per_point, concurrency * meta.agentic_defaults.sessions_per_concurrency),
	]));
}

export function summarizeAgenticLoad(meanInputTokens: number, turns: number, meta: BenchmarkMeta, concurrencies = meta.agentic_defaults.concurrency) {
	const safeTurns = Math.max(2, Math.round(turns));
	const addedInput = meta.agentic_defaults.subsequent_turn_input_tokens;
	const output = meta.agentic_defaults.output_tokens;
	const growth = addedInput + output;
	const firstPromptTokens = Math.round(meanInputTokens - (safeTurns - 1) * growth / 2);
	const prompts = Array.from({ length: safeTurns }, (_, index) => firstPromptTokens + index * growth);
	const promptTokenTotal = prompts.reduce((sum, prompt) => sum + prompt, 0);
	const reusableTokenTotal = prompts.slice(1).reduce((sum, prompt) => sum + Math.max(0, prompt - addedInput), 0);
	const peakPromptTokens = prompts[prompts.length - 1];
	const sessions = Object.values(agenticSessionsPerPoint(meta, concurrencies)).reduce((sum, value) => sum + value, 0);
	return {
		meanInputTokens: Math.round(promptTokenTotal / safeTurns), firstPromptTokens, peakPromptTokens,
		requiredContextTokens: Math.ceil((peakPromptTokens + output + 3_000) / 1_000) * 1_000,
		turns: safeTurns, designPrefixReuseRate: reusableTokenTotal / promptTokenTotal,
		points: concurrencies.length, sessions, requests: sessions * safeTurns,
	};
}

export function formatAgenticTokens(value: number) {
	if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(value % 1_000_000 ? 1 : 0)}M`;
	return `${(value / 1_000).toFixed(value % 1_000 ? 1 : 0)}K`;
}

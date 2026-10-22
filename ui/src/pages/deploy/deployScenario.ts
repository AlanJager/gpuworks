import type { TFunction } from "i18next";

const lowLatency = String.fromCodePoint(0x4f4e, 0x5ef6, 0x8fdf);
const balanced = String.fromCodePoint(0x5747, 0x8861);
const highThroughput = String.fromCodePoint(0x9ad8, 0x541e, 0x5410);

export const DEFAULT_DEPLOY_SCENARIOS = [lowLatency, balanced, highThroughput];

const scenarioKeys = new Map<string, string>([
  [lowLatency, "quick.scenarioValue.latency"],
  [balanced, "quick.scenarioValue.balanced"],
  [highThroughput, "quick.scenarioValue.throughput"],
  ["interactivity", "quick.scenarioValue.latency"],
  ["low_latency", "quick.scenarioValue.latency"],
  ["balanced", "quick.scenarioValue.balanced"],
  ["throughput", "quick.scenarioValue.throughput"],
  ["high_throughput", "quick.scenarioValue.throughput"],
]);

export function deployScenarioLabel(t: TFunction, scenario?: string): string {
  const value = scenario?.trim() ?? "";
  const key = scenarioKeys.get(value) ?? scenarioKeys.get(value.toLowerCase());
  return key ? t(key) : value || t("quick.scenarioValue.unspecified");
}

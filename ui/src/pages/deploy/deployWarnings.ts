import type { TFunction } from "i18next";

import type { ProductWarning } from "../../api";
import { deployScenarioLabel } from "./deployScenario.ts";

// PD_UNVERIFIED_COMBINATION's params.feature names a flag/feature by its
// wire identifier (dgpu, mode, ...), not prose - the
// warningFeature dictionary is the only place those get a reader-facing
// label. A feature this dictionary does not know about yet still shows its
// raw wire value rather than disappearing.
function warningFeatureLabel(t: TFunction<"deploy">, feature: string): string {
  return t(`warningFeature.${feature}`, { defaultValue: feature });
}

export function warningText(
  t: TFunction<"deploy">,
  warning: ProductWarning,
): string {
  const params = { ...warning.params };
  for (const key of ["scenario", "requested_scenario", "selected_scenario"]) {
    if (typeof params[key] === "string")
      params[key] = deployScenarioLabel(t, params[key]);
  }
  if (warning.code === "PD_UNVERIFIED_COMBINATION" && typeof params.feature === "string") {
    params.feature = warningFeatureLabel(t, params.feature);
  }
  return t(`warning.${warning.code}`, {
    ...params,
    defaultValue: warning.code,
  });
}

export function warningKey(warning: ProductWarning): string {
  const params = Object.entries(warning.params ?? {}).sort(([left], [right]) =>
    left.localeCompare(right),
  );
  return `${warning.code}:${JSON.stringify(params)}`;
}

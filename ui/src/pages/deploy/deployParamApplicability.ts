import { createContext, useContext } from "react";
import type {
  EngineParamSpec,
  EngineProfile,
  EvaluateResponse,
} from "../../api";

// Which parameters only make sense for a multimodal model is dictionary data:
// the server marks them `multimodal_only`, and this file keeps no second list
// of key names to fall back on (ADR 0020 decision 1).
function candidateParamSpecs(
  profile: EngineProfile | undefined,
  engine: string,
  modalities: string[] = [],
  groups?: string[],
) {
  const multimodal = modalities.some((modality) =>
    ["image", "audio", "video", "vision"].includes(modality.toLowerCase()),
  );
  return (profile?.param_specs ?? [])
    .filter(
      (spec) =>
        (multimodal || !spec.multimodal_only) &&
        (!spec.engines?.length || spec.engines.includes(engine)),
    )
    .filter((spec) => !groups?.length || groups.includes(spec.group || ""));
}

export function visibleParamSpecs(
  profile: EngineProfile | undefined,
  engine: string,
  modalities: string[] = [],
  groups?: string[],
) {
  return candidateParamSpecs(profile, engine, modalities, groups);
}

export function quickParamSpecs(
  profile: EngineProfile | undefined,
  engine: string,
  modalities: string[] = [],
  templateKeys?: string[],
) {
  const keys = templateKeys?.length
    ? templateKeys
    : (profile?.quick_form_keys ?? []);
  const visible = new Map(
    visibleParamSpecs(profile, engine, modalities).map((spec) => [
      spec.key,
      spec,
    ]),
  );
  return keys.flatMap((key) => {
    const spec = visible.get(key);
    return spec ? [spec] : [];
  });
}

// The server names the keys whose dictionary default this launch leaves
// unstated - its entrypoint or engine release may refuse the flag - and the
// form starts them at "automatic" instead of the default. Nothing is hidden
// on that judgement: the deployer can still turn each one on or off. A
// completed evaluation is authoritative even when the server omits an empty
// list from JSON; before it, the template projection gives the form a useful
// initial shape.
export function effectiveEngineDecidesParamKeys(
  result: EvaluateResponse | null | undefined,
  templateKeys: string[] = [],
): string[] {
  return result ? (result.engine_decides_param_keys ?? []) : templateKeys;
}

export const EngineDecidesContext = createContext<readonly string[]>([]);

// useEngineDecides answers whether a control's unstated position is the
// engine's choice rather than the dictionary default: always for a key the
// dictionary gives no default, and for a key this launch leaves to the
// engine (EngineDecidesContext).
export function useEngineDecides(spec: EngineParamSpec): boolean {
  const keys = useContext(EngineDecidesContext);
  return (
    spec.default === undefined ||
    spec.default === null ||
    keys.includes(spec.key)
  );
}

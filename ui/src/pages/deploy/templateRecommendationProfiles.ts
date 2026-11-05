import type { InferenceProfileSummary, InferenceTemplateIndexItem, Model } from "../../api";

export function matchingTemplates(
  templates: InferenceTemplateIndexItem[],
  model?: Model,
  scenario?: string,
) {
  if (!model) return [];
  const identities = [model.source_id, model.name]
    .filter(Boolean)
    .map((value) => value.toLowerCase());
  return templates
    .filter(
      (template) =>
        identities.includes(template.model_id.toLowerCase()) ||
        (template.profiles || []).some((profile) =>
          (profile.weight_repo_ids || []).some((repoID) =>
            identities.includes(repoID.toLowerCase()),
          ),
        ),
    )
    .filter((template) => !scenario || template.scenarios.includes(scenario));
}

// Hardware compatibility is the server projection for the selected scope.
export function matchingProfiles(
  templates: InferenceTemplateIndexItem[],
  model?: Model,
  scenario?: string,
  targetVendor?: string,
): Array<{
  template: InferenceTemplateIndexItem;
  profile: InferenceProfileSummary;
  compatible: boolean;
}> {
  return matchingTemplates(templates, model, scenario)
    .flatMap((template) =>
      (template.profiles || []).map((profile) => ({
        template,
        profile,
        compatible: profile.detected_compatible === true,
      })),
    )
    .filter(
      ({ profile }) =>
        !targetVendor ||
        String(profile.gpus.vendor || "")
          .trim()
          .toLowerCase() ===
          String(targetVendor || "")
            .trim()
            .toLowerCase(),
    );
}

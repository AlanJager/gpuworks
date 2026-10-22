import { Button, Empty, Space, Tag, Typography } from "antd";
import { CheckCircleOutlined } from "@ant-design/icons";
import type {
  DetectedTemplateChip,
  InferenceProfileSummary,
  InferenceTemplateIndexItem,
  Model,
} from "../../api";
import { useTranslation } from "react-i18next";
import {
  chipDisplayLabel,
  gpuPredicateTargetName,
} from "../inferenceTemplateFlat";
import { chipMatchesPredicate } from "../inferenceTemplateFlat.ts";

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

// matchingProfiles marks a template profile compatible when one of the
// chips - the card models the online nodes have (GET /nodes/facets) -
// matches its GPU predicate.
export function matchingProfiles(
  templates: InferenceTemplateIndexItem[],
  chips: DetectedTemplateChip[],
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
        compatible:
          Boolean(profile.gpus) &&
          chips.some((chip) => chipMatchesPredicate(chip, profile.gpus)),
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

function predicateLabel(
  profile: InferenceProfileSummary,
  t: (key: string, options?: Record<string, unknown>) => string,
) {
  return gpuPredicateTargetName(profile.gpus, t);
}

function currentHardware(chips: DetectedTemplateChip[]) {
  const seen = new Set<string>();
  return (
    chips
      .filter((chip) => {
        const key = `${chip.vendor}\u0000${chip.name}\u0000${chip.cc || ""}`;
        if (seen.has(key)) return false;
        seen.add(key);
        return true;
      })
      .map(chipDisplayLabel)
      .join(" · ") || "No online GPU reported"
  );
}

function profileMeta(
  profile: InferenceProfileSummary,
  t: (key: string, options?: Record<string, unknown>) => string,
) {
  return (
    [
      profile.quantization,
      profile.weight_bytes
        ? `${(profile.weight_bytes / 1024 ** 3).toFixed(1)} GiB`
        : "",
    ]
      .filter(Boolean)
      .join(" · ") || t("templateRecommendations.defaults")
  );
}

export default function TemplateRecommendations({
  templates,
  chips,
  model,
  scenario,
  targetVendor,
  selectedID,
  selectedSpecName,
  onSelect,
}: {
  templates: InferenceTemplateIndexItem[];
  chips: DetectedTemplateChip[];
  model?: Model;
  scenario?: string;
  targetVendor?: string;
  selectedID?: number;
  selectedSpecName?: string;
  onSelect: (
    template: InferenceTemplateIndexItem,
    profile: InferenceProfileSummary,
  ) => void;
}) {
  const { t } = useTranslation("deploy");
  const allProfiles = matchingProfiles(
    templates,
    chips,
    model,
    scenario,
    targetVendor,
  );
  const matches = allProfiles.filter((item) => item.compatible);
  if (!model)
    return (
      <Typography.Text type="secondary">
        {t("templateRecommendations.selectModel")}
      </Typography.Text>
    );
  if (!targetVendor)
    return (
      <Typography.Text type="secondary">
        {t("templateRecommendations.selectVendor")}
      </Typography.Text>
    );
  const hidden = allProfiles.filter((item) => !item.compatible);
  const hardware = currentHardware(chips);
  if (!allProfiles.length)
    return (
      <Empty
        image={Empty.PRESENTED_IMAGE_SIMPLE}
        description={t("templateRecommendations.empty")}
      />
    );
  return (
    <div>
      {!matches.length && (
        <Typography.Text type="secondary">
          {t("templateRecommendations.empty")}
        </Typography.Text>
      )}
      <div className="deploy-template-grid">
        {matches.map(({ template, profile }) => {
          const selected =
            template.id === selectedID && profile.name === selectedSpecName;
          return (
            <Button
              key={`${template.id}:${profile.name}`}
              className={`deploy-template-option${selected ? " is-selected" : ""}`}
              onClick={() => onSelect(template, profile)}
            >
              <Space direction="vertical" size={3} align="start">
                <Space>
                  <Typography.Text strong>
                    {template.display_title || template.model_id}
                  </Typography.Text>
                  {selected && <CheckCircleOutlined />}
                </Space>
                <Typography.Text type="secondary">
                  {profile.display_profile || profile.name} · {profile.engine} ·{" "}
                  {profile.scenario || t("templateRecommendations.general")}
                </Typography.Text>
                <Typography.Text type="secondary">
                  {t("templateRecommendations.requires")}:{" "}
                  <Typography.Text code>
                    {predicateLabel(profile, t)}
                  </Typography.Text>
                </Typography.Text>
                <Typography.Text type="secondary">
                  {profileMeta(profile, t)}
                </Typography.Text>
                <Tag color="green">
                  {t("templateRecommendations.deployable")}
                </Tag>
              </Space>
            </Button>
          );
        })}
      </div>
      <Typography.Text type="secondary" className="deploy-template-hardware">
        {t("templateRecommendations.currentHardware")}: {hardware}
      </Typography.Text>
      {hidden.length > 0 && (
        <details className="deploy-template-unavailable">
          <summary>
            {t("templateRecommendations.otherProfiles", {
              count: hidden.length,
            })}
          </summary>
          {hidden.map(({ template, profile }) => (
            <Typography.Paragraph
              key={`${template.id}:${profile.name}`}
              type="secondary"
            >
              <strong>{template.display_title || template.model_id}</strong> ·{" "}
              {profile.display_profile || profile.name} · {profile.engine} ·{" "}
              {profileMeta(profile, t)}
              <br />
              {t("templateRecommendations.requires")}:{" "}
              <Typography.Text code>
                {predicateLabel(profile, t)}
              </Typography.Text>
              <br />
              {t("templateRecommendations.currentHardware")}: {hardware} ·{" "}
              {t("templateRecommendations.chipUnavailable")}
            </Typography.Paragraph>
          ))}
        </details>
      )}
    </div>
  );
}

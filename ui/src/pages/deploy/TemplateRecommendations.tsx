import { useEffect, useState } from "react";
import { api, type InferenceTemplateIndex } from "../../api";
import { Button, Empty, Space, Spin, Tag, Typography } from "antd";
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
  predicateLabel,
  specDisplayName,
} from "../inferenceTemplateFlat";

import { matchingProfiles } from "./templateRecommendationProfiles";
export { matchingProfiles, matchingTemplates } from "./templateRecommendationProfiles";

function currentHardware(chips: DetectedTemplateChip[]) {
  const seen = new Set<string>();
  return (
    chips
      .filter((chip) => {
        const key = `${chip.vendor}\u0000${chip.name}\u0000${chip.cc || ""}\u0000${chip.arch_id || ""}`;
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
  nodeUUID,
  model,
  scenario,
  targetVendor,
  selectedID,
  selectedSpecName,
  onSelect,
}: {
  nodeUUID?: string;
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
  const { t, i18n } = useTranslation("deploy");
  const scopeKey = `${targetVendor || ""}\u0000${nodeUUID || ""}`;
  const [support, setSupport] = useState<{ key: string; index?: InferenceTemplateIndex; error?: string }>();
  const hasModel = Boolean(model);
  useEffect(() => {
    if (!targetVendor || !hasModel) { setSupport(undefined); return; }
    let active = true;
    setSupport({ key: scopeKey });
    void api.inferenceTemplateIndex({ vendor: targetVendor, node_uuid: nodeUUID }).then(
      (index) => { if (active) setSupport({ key: scopeKey, index }); },
      (reason) => { if (active) setSupport({ key: scopeKey, error: (reason as Error).message }); },
    );
    return () => { active = false; };
  }, [targetVendor, nodeUUID, scopeKey, hasModel]);
  const currentSupport = support?.key === scopeKey ? support : undefined;
  const chips = currentSupport?.index?.hardware || [];
  const allProfiles = matchingProfiles(
    currentSupport?.index?.items || [],
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
  if (currentSupport?.error) return <Typography.Text type="danger">{t("templateRecommendations.loadFailed", { detail: currentSupport.error })}</Typography.Text>;
  if (!currentSupport?.index) return <Spin />;
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
                  {specDisplayName(profile, i18n.language)} · {profile.engine} ·{" "}
                  {profile.scenario || t("templateRecommendations.general")}
                </Typography.Text>
                <Typography.Text type="secondary">
                  {t("templateRecommendations.requires")}:{" "}
                  <Typography.Text code>
                    {predicateLabel(profile)}
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
        <br />{t("templateRecommendations.compatibilityScope")}
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
              {specDisplayName(profile, i18n.language)} · {profile.engine} ·{" "}
              {profileMeta(profile, t)}
              <br />
              {t("templateRecommendations.requires")}:{" "}
              <Typography.Text code>
                {predicateLabel(profile)}
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

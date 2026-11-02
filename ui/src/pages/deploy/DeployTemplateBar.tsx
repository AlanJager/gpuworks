import { Button, Form, Tag, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { specDisplayName } from "../inferenceTemplateFlat";
import type { WeightModel } from "./useTemplateWeightSource";
import { ModelSourceTag } from "../ModelsPageParts";
import type { TemplateSpecSelection } from "./deployHelpers";

export default function DeployTemplateBar({
  selection,
  changed,
  evaluatedImage,
  onDetails,
  weightModel,
}: {
  selection: TemplateSpecSelection;
  changed?: boolean;
  evaluatedImage?: string;
  onDetails: () => void;
  /** The registered model that now provides the weights, once chosen. */
  weightModel?: WeightModel;
}) {
  const { t, i18n } = useTranslation("deploy");
  const form = Form.useFormInstance();
  const imageOverride = Form.useWatch("image", { form, preserve: true }) as
    string | undefined;
  const spec = selection.spec;
  const currentImage =
    imageOverride ||
    evaluatedImage ||
    spec.image ||
    t("templateLock.imagePending");
  return (
    <section className="deploy-template-lockbar">
      <div>
        <Typography.Text strong>
          {spec.weight_sources[0]?.repo_id}
        </Typography.Text>
        <Typography.Text
          type={
            changed === undefined
              ? "secondary"
              : changed
                ? "warning"
                : "success"
          }
        >
          {changed === undefined
            ? t("catalog.evaluationPending")
            : changed
              ? t("templateLock.deviated")
              : t("templateLock.usingPreset")}
        </Typography.Text>
      </div>
      {weightModel && (
        <Typography.Text>
          {t("templateLock.weightSource")}: <ModelSourceTag source={weightModel.source} />
          {weightModel.source_id}
        </Typography.Text>
      )}
      <Tag color="purple">
        {spec.quantization} ·{" "}
        {spec.weight_bytes
          ? `${(spec.weight_bytes / 1024 ** 3).toFixed(1)} GiB`
          : t("templateLock.sizePending")}
      </Tag>
      <Tag color="red">{specDisplayName(spec, i18n.language)}</Tag>
      <Tag color="blue">{spec.engine}</Tag>
      <Typography.Text className="deploy-locked-image" copyable>
        {t("catalog.currentImage")}: {currentImage}
      </Typography.Text>
      {spec.image && spec.image !== currentImage && (
        <Typography.Text type="secondary">
          {t("catalog.templateImage")}: {spec.image}
        </Typography.Text>
      )}
      <Tag>
        {t("templateLock.scenario", {
          value: spec.scenario || t("templateLock.general"),
        })}
      </Tag>
      <Button type="link" onClick={onDetails}>
        {t("templateLock.details")}
      </Button>
    </section>
  );
}

import { Button, message } from "antd";
import type { InferenceTemplate } from "../../api";
import { api } from "../../api";
import { useTranslation } from "react-i18next";

// The page's action bar: the selected template's clone affordance and the
// deploy button, gated on a settled evaluation.
export default function DeploySubmitBar({
  selectedTemplate,
  submit,
  submitting,
  disabled,
}: {
  selectedTemplate?: InferenceTemplate;
  submit: () => void;
  submitting: boolean;
  disabled: boolean;
}) {
  const { t } = useTranslation("deploy");
  const clone = async () => {
    if (!selectedTemplate) return;
    try {
      await api.cloneInferenceTemplate(selectedTemplate.model_id);
      message.success(t("template.cloneSuccess"));
    } catch (e) {
      message.error(
        t("template.cloneFailed", { detail: (e as Error).message }),
      );
    }
  };
  return (
    <div className="deploy-submit-bar">
      {selectedTemplate && (
        <Button onClick={clone}>{t("template.clone")}</Button>
      )}
      <Button
        type="primary"
        aria-label={t("page.deploy")}
        aria-busy={submitting}
        onClick={submit}
        loading={submitting}
        disabled={disabled}
      >
        {t("page.deploy")}
      </Button>
    </div>
  );
}

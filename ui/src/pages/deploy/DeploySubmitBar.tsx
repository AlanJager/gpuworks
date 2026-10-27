import { Button, Typography, message } from "antd";
import type { InferenceTemplate } from "../../api";
import { api } from "../../api";
import { useTranslation } from "react-i18next";

// The page's action bar: the selected template's clone affordance and the
// deploy button, gated on a settled evaluation. Editing a deployment swaps the
// button for "Review changes", which waits for there to be changes to review.
export default function DeploySubmitBar({
  selectedTemplate,
  submit,
  submitting,
  disabled,
  edit,
}: {
  selectedTemplate?: InferenceTemplate;
  submit: () => void;
  submitting: boolean;
  disabled: boolean;
  edit?: { changes: number };
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
      {edit && (
        <Typography.Text type="secondary" role="status">
          {t("edit.changeCount", { count: edit.changes })}
        </Typography.Text>
      )}
      {selectedTemplate && !edit && (
        <Button onClick={clone}>{t("template.clone")}</Button>
      )}
      <Button
        type="primary"
        aria-label={edit ? t("edit.review") : t("page.deploy")}
        aria-busy={submitting}
        onClick={submit}
        loading={submitting}
        disabled={disabled || (edit !== undefined && edit.changes === 0)}
      >
        {edit ? t("edit.review") : t("page.deploy")}
      </Button>
    </div>
  );
}

import { Alert } from "antd";
import { useTranslation } from "react-i18next";

import type { InferenceTemplateIssue } from "../api";

// The non-blocking findings the server returns for a template that imported
// (POST /inference-templates/import `warnings`). Each line is the localized
// text of its code with the warning's params interpolated; a code this
// console has no text for shows the server's English text instead. Nothing is
// rendered when there are no warnings.
export default function TemplateWarnings({ warnings, onClose }: { warnings: InferenceTemplateIssue[]; onClose?: () => void }) {
  const { t } = useTranslation("inferenceTemplates");
  if (warnings.length === 0) return null;
  return <Alert
    className="template-import-warnings"
    type="warning"
    showIcon
    closable={Boolean(onClose)}
    onClose={onClose}
    message={t("warnings.title", { count: warnings.length })}
    description={<ul>
      {warnings.map((warning, index) => <li key={`${warning.code}:${index}`}>{t(`warnings.${warning.code}`, { ...warning.params, defaultValue: warning.error })}</li>)}
    </ul>}
  />;
}

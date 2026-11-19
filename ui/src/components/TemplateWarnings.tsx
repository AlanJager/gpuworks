import { useTranslation } from "react-i18next";
import { Button } from "./ui/button";
import { Notice } from "../ui-kit/Notice";
import { ConsoleIcon } from "../ui-kit/consoleIcons";

import type { InferenceTemplateIssue } from "../api";

// The non-blocking findings the server returns for a template that imported
// (POST /inference-templates/import `warnings`). Each line is the localized
// text of its code with the warning's params interpolated; a code this
// console has no text for shows the server's English text instead. Nothing is
// rendered when there are no warnings.
export default function TemplateWarnings({ warnings, onClose }: { warnings: InferenceTemplateIssue[]; onClose?: () => void }) {
  const { t } = useTranslation("inferenceTemplates");
  const { t: tc } = useTranslation("common");
  if (warnings.length === 0) return null;
  return <Notice
    className="template-import-warnings"
    tone="warning"
    role="alert"
    title={t("warnings.title", { count: warnings.length })}
    action={onClose && <Button variant="ghost" size="xs" iconOnly aria-label={tc("actions.close")} onClick={onClose}><ConsoleIcon.close size={16} aria-hidden /></Button>}
  >
    <ul className="m-0 list-disc pl-5">
      {warnings.map((warning, index) => <li key={`${warning.code}:${index}`}>{t(`warnings.${warning.code}`, { ...warning.params, defaultValue: warning.error })}</li>)}
    </ul>
  </Notice>;
}

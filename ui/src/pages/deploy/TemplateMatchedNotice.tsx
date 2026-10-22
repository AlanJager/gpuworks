import { Alert, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { EvaluateResponse } from "../../api";

export default function TemplateMatchedNotice({
  result,
}: {
  result?: EvaluateResponse | null;
  onSwitch?: () => void;
}) {
  const { t } = useTranslation("deploy");
  if (!result?.matched_by) return null;
  return (
    <Alert
      type="info"
      showIcon
      message={t("confirm.templateMatch.configured", {
        label: result.template_spec_name || "spec",
      })}
      description={
        <Typography.Text type="secondary" title={result.matched_by}>
          {result.matched_by}
        </Typography.Text>
      }
    />
  );
}

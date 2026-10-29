import { Alert, Typography } from "antd";
import { useTranslation } from "react-i18next";

/**
 * BatchFileModels says which models the chosen requests file names, read in
 * the browser. Without a chosen model the ones no active deployment serves are
 * a warning (their requests would fail); with one, every model that differs is
 * only said to be replaced. It never blocks the upload.
 */
export default function BatchFileModels({ fileModels, skipped, known, override }: {
  /** The distinct models in the file; null until read, or when the file is not read. */
  fileModels: string[] | null;
  /** The file is over the size the browser reads. */
  skipped: boolean;
  /** Served names of the active deployments; null when they could not be listed. */
  known: Set<string> | null;
  override: string | undefined;
}) {
  const { t } = useTranslation("batches");
  if (skipped) return <Typography.Text type="secondary">{t("create.fileModels.skipped")}</Typography.Text>;
  if (!fileModels) return null;
  const summary = (
    <Typography.Text type="secondary">
      {fileModels.length > 0 ? t("create.fileModels.count", { count: fileModels.length, models: fileModels.join(", ") }) : t("create.fileModels.none")}
    </Typography.Text>
  );
  if (override) {
    const replaced = fileModels.filter((name) => name !== override);
    return (
      <>
        {summary}
        {replaced.length > 0 && <Alert type="info" showIcon message={t("create.fileModels.replaced", { count: replaced.length, models: replaced.join(", "), model: override })} />}
      </>
    );
  }
  const unknown = known ? fileModels.filter((name) => !known.has(name)) : [];
  return (
    <>
      {summary}
      {unknown.length > 0 && <Alert type="warning" showIcon message={t("create.fileModels.unknown", { count: unknown.length, models: unknown.join(", ") })} />}
    </>
  );
}

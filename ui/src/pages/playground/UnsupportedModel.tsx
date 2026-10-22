import { Alert, Space } from "antd";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { playgroundRoute } from "./route";
import type { PlaygroundModel } from "./types";

export default function UnsupportedModel({ modelID, models }: { modelID: string; models: PlaygroundModel[] }) {
  const { t } = useTranslation("playground");
  const model = models.find((item) => item.id === modelID);
  return <Alert type="warning" showIcon message={t("page.unsupportedModel", { model: modelID })} description={
    <Space wrap>{model?.operations.map((operation) => <Link key={operation.type} to={playgroundRoute(operation, modelID)}>{operation.type}</Link>)}</Space>
  } />;
}

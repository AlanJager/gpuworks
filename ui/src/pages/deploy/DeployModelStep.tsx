import { Col, Divider, Row, Typography } from "antd";
import type { EngineProfile, Model } from "../../api";
import DeployParamField from "./DeployParamField";
import { visibleParamSpecs } from "./deployHelpers";
import { useTranslation } from "react-i18next";

export default function DeployModelStep({
  profile,
  engine,
  model,
}: {
  profile?: EngineProfile;
  engine: string;
  model?: Model;
}) {
  const { t } = useTranslation("deploy");
  const modelSpecs = visibleParamSpecs(
    profile,
    engine,
    model?.input_modalities,
    ["model"],
  );
  const multimodalSpecs = visibleParamSpecs(
    profile,
    engine,
    model?.input_modalities,
    ["multimodal"],
  );
  return (
    <div>
      <Typography.Title level={4}>{t("modelStep.title")}</Typography.Title>
      <Row gutter={16}>
        {modelSpecs.map((spec) => (
          <Col xs={24} md={12} key={spec.key}>
            <DeployParamField spec={spec} />
          </Col>
        ))}
      </Row>
      {multimodalSpecs.length > 0 && (
        <>
          <Divider orientation="left">{t("modelStep.multimodal")}</Divider>
          <Row gutter={16}>
            {multimodalSpecs.map((spec) => (
              <Col xs={24} md={12} key={spec.key}>
                <DeployParamField spec={spec} />
              </Col>
            ))}
          </Row>
        </>
      )}
    </div>
  );
}

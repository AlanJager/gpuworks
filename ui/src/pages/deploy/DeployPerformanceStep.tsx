import {
  Alert,
  Col,
  Collapse,
  Divider,
  Form,
  Row,
  Space,
  Switch,
  Typography,
} from "antd";
import type { EngineProfile, Model } from "../../api";
import DeployParamField from "./DeployParamField";
import {
  kvTierSpecs,
  memoryTierEnabledField,
  memoryTierKey,
  visibleParamSpecs,
} from "./deployHelpers";
import { useTranslation } from "react-i18next";

export default function DeployPerformanceStep({
  profile,
  engine,
  model,
}: {
  profile?: EngineProfile;
  engine: string;
  model?: Model;
}) {
  const { t } = useTranslation("deploy");
  const form = Form.useFormInstance();
  const specs = visibleParamSpecs(
    profile,
    engine,
    model?.input_modalities,
    ["performance"],
  );
  const speculative = visibleParamSpecs(
    profile,
    engine,
    model?.input_modalities,
    ["speculative"],
  );
  const advanced = visibleParamSpecs(
    profile,
    engine,
    model?.input_modalities,
    ["performance_advanced"],
  );
  // The dictionary's depends_on relation owns tier membership; kvTierSpecs is
  // its one reader.
  const { memory: memorySpec, disk: diskSpec, companions: diskCompanions } =
    kvTierSpecs(specs);
  // The tier switches set fields whose Form.Item only mounts once the tier is
  // on, so they must watch the store itself (preserve) or they would never see
  // the value they just wrote and could not be turned on at all. The disk
  // tier's switch is the parameter itself; the memory tier keeps a card
  // position so its edited size survives the tier being off.
  const memoryValue = Number(
    Form.useWatch(memoryTierKey, { form, preserve: true }) || 0,
  );
  const diskValue = Boolean(
    Form.useWatch("kv_disk_offload", { form, preserve: true }),
  );
  const memoryIntent = Form.useWatch(memoryTierEnabledField, {
    form,
    preserve: true,
  });
  const memoryDesired =
    memoryIntent === undefined ? memoryValue > 0 : Boolean(memoryIntent);
  const memoryEnabled = memoryDesired && memoryValue > 0;
  const diskEnabled = memoryEnabled && diskValue;
  const tierAuxiliary = new Set([
    memoryTierKey,
    "kv_disk_offload",
    ...diskCompanions.map((spec) => spec.key),
  ]);
  const regular = specs.filter((spec) => !tierAuxiliary.has(spec.key));
  return (
    <div>
      <Typography.Title level={4}>{t("performance.title")}</Typography.Title>
      <Row gutter={16}>
        {regular.map((spec) => (
          <Col
            xs={24}
            md={
              spec.control === "segmented" || spec.control === "slider"
                ? 24
                : 12
            }
            key={spec.key}
          >
            <DeployParamField spec={spec} />
          </Col>
        ))}
      </Row>
      {memorySpec && (
        <>
          <Divider orientation="left">{t("performance.kvTiers")}</Divider>
          <div className="deploy-kv-tier">
            <Typography.Text strong>{t("performance.gpuTier")}</Typography.Text>
            <Typography.Text type="secondary">
              {t("performance.gpuTierHelp")}
            </Typography.Text>
          </div>
          <div className="deploy-kv-tier">
            <Space style={{ width: "100%", justifyContent: "space-between" }}>
              <div>
                <Typography.Text strong>
                  {t("performance.cpuTier")}
                </Typography.Text>
                <br />
                <Typography.Text type="secondary">
                  {t("performance.cpuTierHelp")}
                </Typography.Text>
              </div>
              <Switch
                aria-label={t("performance.cpuTier")}
                checked={memoryDesired}
                onChange={(checked) =>
                  form.setFieldValue(memoryTierEnabledField, checked)
                }
              />
            </Space>
            {memoryDesired && (
              <DeployParamField
                spec={memorySpec}
                rules={[
                  {
                    required: true,
                    message: t("performance.memoryCapacityRequired"),
                  },
                  {
                    validator: (_, value) =>
                      Number(value) > 0
                        ? Promise.resolve()
                        : Promise.reject(
                            new Error(t("performance.memoryCapacityRequired")),
                          ),
                  },
                ]}
              />
            )}
          </div>
          {diskSpec && (
            <div className="deploy-kv-tier">
              <Space style={{ width: "100%", justifyContent: "space-between" }}>
                <div>
                  <Typography.Text strong>
                    {t("performance.diskTier")}
                  </Typography.Text>
                  <br />
                  <Typography.Text type="secondary">
                    {t("performance.diskTierHelp")}
                  </Typography.Text>
                </div>
                <Switch
                  aria-label={t("performance.diskTier")}
                  checked={diskValue}
                  disabled={!memoryEnabled}
                  onChange={(checked) =>
                    form.setFieldValue("kv_disk_offload", checked)
                  }
                />
              </Space>
              {diskEnabled && (
                <Row gutter={16} style={{ marginTop: 12 }}>
                  {diskCompanions.map((spec) => (
                    <Col
                      xs={24}
                      md={spec.control === "path" ? 24 : 12}
                      key={spec.key}
                    >
                      <DeployParamField spec={spec} />
                    </Col>
                  ))}
                </Row>
              )}
            </div>
          )}
        </>
      )}
      {speculative.length > 0 && (
        <>
          <Divider orientation="left">{t("performance.speculative")}</Divider>
          <Row gutter={16}>
            {speculative.map((spec) => (
              <Col xs={24} md={12} key={spec.key}>
                <DeployParamField spec={spec} />
              </Col>
            ))}
          </Row>
        </>
      )}
      {advanced.length > 0 && (
        <Collapse
          ghost
          items={[
            {
              key: "advanced",
              label: t("performance.advanced"),
              children: (
                <Row gutter={16}>
                  {advanced.map((spec) => (
                    <Col xs={24} md={12} key={spec.key}>
                      <DeployParamField spec={spec} />
                    </Col>
                  ))}
                </Row>
              ),
            },
          ]}
        />
      )}
      {engine === "sglang" && (
        <Alert type="info" showIcon message={t("performance.sglangNotice")} />
      )}
    </div>
  );
}

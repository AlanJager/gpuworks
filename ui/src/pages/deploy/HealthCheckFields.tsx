import { useEffect } from "react";
import { Checkbox, Form, InputNumber, Space } from "antd";
import { useTranslation } from "react-i18next";
import type { Model } from "../../api";
import {
  HealthCheckHelp,
  healthCheckable,
} from "../../components/DeploymentHealth";

// The server's bounds for health_check_threshold_sec
// (store.HealthCheckThresholdSec*) and the threshold it takes when none is
// stated.
const thresholdMin = 1;
const thresholdMax = 600;
const thresholdDefault = 60;

// HealthCheckBox is health_check's control. Unstated, the server turns the
// check on for a model that serves chat, text completion or embeddings, so the box shows
// exactly that; a model that serves neither cannot be checked.
function HealthCheckBox({
  value,
  onChange,
  checkable,
}: {
  value?: boolean;
  onChange?: (value: boolean | undefined) => void;
  checkable: boolean;
}) {
  const { t } = useTranslation("common");
  useEffect(() => {
    // A choice made for a model that could be checked does not carry over
    // to one that cannot.
    if (!checkable && value === true) onChange?.(undefined);
  }, [checkable, value, onChange]);
  return (
    <Checkbox
      checked={checkable && (value ?? true)}
      disabled={!checkable}
      onChange={(e) => onChange?.(e.target.checked)}
    >
      {t("healthCheck.enable")}
    </Checkbox>
  );
}

// HealthCheckFields are the deployment's health check settings: on or off,
// and how long a check waits.
export default function HealthCheckFields({
  selectedModel,
}: {
  selectedModel?: Model;
}) {
  const { t } = useTranslation("common");
  const form = Form.useFormInstance();
  const stated = Form.useWatch("health_check", { form, preserve: true }) as
    boolean | undefined;
  const checkable = healthCheckable(selectedModel?.operations);
  const on = checkable && (stated ?? true);
  return (
    <Form.Item
      label={
        <Space size={4}>
          {t("healthCheck.title")}
          <HealthCheckHelp />
        </Space>
      }
      extra={
        selectedModel && !checkable ? t("healthCheck.unsupported") : undefined
      }
    >
      <Space wrap size={12}>
        <Form.Item name="health_check" noStyle>
          <HealthCheckBox checkable={checkable} />
        </Form.Item>
        <Form.Item name="health_check_threshold_sec" noStyle>
          <InputNumber
            aria-label={t("healthCheck.threshold")}
            addonBefore={t("healthCheck.threshold")}
            addonAfter={t("healthCheck.seconds")}
            min={thresholdMin}
            max={thresholdMax}
            precision={0}
            placeholder={String(thresholdDefault)}
            disabled={!on}
            style={{ width: 200 }}
          />
        </Form.Item>
      </Space>
    </Form.Item>
  );
}

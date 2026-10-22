// KV cache offload tier picker: GPU-resident vs. host-memory vs.
// host-memory+disk, the memory capacity field, and the disk tier's
// companion parameters.
import { Form } from "antd";
import { useTranslation } from "react-i18next";
import type { EngineParamSpec } from "../../api";
import DeployParamField from "./DeployParamField";
import { ChoiceCard } from "./DeployChoiceCards";
import { memoryTierEnabledField } from "./deployHelpers";

type Props = {
  kvMemorySpec?: EngineParamSpec;
  kvDiskSpec?: EngineParamSpec;
  kvDiskCompanionSpecs: EngineParamSpec[];
  manualCommand: boolean;
};

export default function DeployKvOffloadSection({
  kvMemorySpec,
  kvDiskSpec,
  kvDiskCompanionSpecs,
  manualCommand,
}: Props) {
  const { t } = useTranslation("deploy");
  const form = Form.useFormInstance();
  const kvMemoryValue = Number(
    Form.useWatch(kvMemorySpec?.key || "__missing_kv_memory", {
      form,
      preserve: true,
    }) || 0,
  );
  const kvDiskValue = Boolean(
    Form.useWatch(kvDiskSpec?.key || "__missing_kv_disk", {
      form,
      preserve: true,
    }),
  );
  // The card position is explicit once the deployer clicks; before that the
  // tier follows its value, so a template-provided size lights the card.
  const kvMemoryIntent = Form.useWatch(memoryTierEnabledField, {
    form,
    preserve: true,
  });
  const kvMemoryDesired =
    kvMemoryIntent === undefined ? kvMemoryValue > 0 : Boolean(kvMemoryIntent);
  const kvMemoryEnabled = kvMemoryDesired && kvMemoryValue > 0;
  // The disk tier depends on the memory tier, so its card and companions go
  // dark with it even though their draft values survive.
  const kvDiskEnabled = kvMemoryEnabled && kvDiskValue;

  return (
    <div className="deploy-workbench-row">
      <div className="deploy-workbench-label">KV Offload</div>
      <div className="deploy-workbench-body">
        <div className="deploy-choice-strip">
          <ChoiceCard
            active={!kvMemoryDesired}
            disabled={manualCommand}
            title={t("workbench.kv.disabled")}
            description={t("workbench.kv.gpuResident")}
            onClick={() => form.setFieldValue(memoryTierEnabledField, false)}
          />
          <ChoiceCard
            active={kvMemoryDesired && !kvDiskEnabled}
            disabled={!kvMemorySpec || manualCommand}
            title={t("workbench.kv.memory")}
            description={
              kvMemorySpec
                ? kvMemoryValue > 0
                  ? `${kvMemoryValue} GiB`
                  : t("workbench.kv.enterCapacity")
                : t("workbench.engineUnavailable")
            }
            onClick={() =>
              kvMemorySpec && form.setFieldValue(memoryTierEnabledField, true)
            }
          />
          <ChoiceCard
            active={kvDiskEnabled}
            disabled={!kvDiskSpec || !kvMemoryEnabled || manualCommand}
            title={t("workbench.kv.disk")}
            description={
              kvDiskSpec
                ? !kvMemoryEnabled
                  ? t("workbench.kv.memoryRequired")
                  : kvDiskEnabled
                    ? t("workbench.enabled")
                    : t("workbench.clickToEnable")
                : t("workbench.engineUnavailable")
            }
            onClick={() => {
              if (!kvDiskSpec || !kvMemoryEnabled) return;
              // The disk tier's switch is the parameter itself: on states it true,
              // off states false, and the value-editable companions ride along.
              form.setFieldValue(kvDiskSpec.key, !kvDiskEnabled);
            }}
          />
        </div>
        {kvMemorySpec && kvMemoryDesired && (
          <div className="deploy-inline-field">
            <DeployParamField
              spec={kvMemorySpec}
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
          </div>
        )}
        {kvDiskSpec && kvDiskEnabled && kvDiskCompanionSpecs.length > 0 && (
          <div className="deploy-resource-grid">
            {kvDiskCompanionSpecs.map((spec: EngineParamSpec) => (
              <DeployParamField key={spec.key} spec={spec} />
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

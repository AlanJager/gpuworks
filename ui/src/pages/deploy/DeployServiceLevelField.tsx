import { useContext } from "react";
import { Form, Select } from "antd";
import { useTranslation } from "react-i18next";
import { yieldPresets } from "../yieldPolicy/model";
import { DeployEditContext } from "./DeployEditContext";

// DeployServiceLevelField picks the yield policy preset a new deployment starts
// with, which decides how it shares its GPUs with the others on them (resident
// by default). It shows only for a deployment the server accepts a policy
// other than resident for - sleepable, single-node, not PD - and only when
// creating: an existing deployment's policy is changed on its scheduling
// policy card, where its parameters can be tuned too.
export default function DeployServiceLevelField() {
  const { t } = useTranslation("deploy");
  const form = Form.useFormInstance();
  const { mode } = useContext(DeployEditContext);
  const watch = { form, preserve: true } as const;
  const sleepable = Boolean(Form.useWatch("sleepable", watch));
  const nnodes = Number(Form.useWatch("nnodes", watch) || 1);
  const nodeMode = String(Form.useWatch("node_selection_mode", watch) || "");
  if (mode === "edit" || !sleepable || nnodes > 1 || nodeMode === "pd") return null;
  return (
    <Form.Item
      label={t("serviceLevel.label")}
      name="yield_preset"
      initialValue="resident"
      extra={t("serviceLevel.help")}
    >
      <Select
        aria-label={t("serviceLevel.label")}
        optionLabelProp="short"
        options={yieldPresets.map((value) => ({
          value,
          short: t(`serviceLevel.name.${value}`),
          label: `${t(`serviceLevel.name.${value}`)} - ${t(`serviceLevel.hint.${value}`)}`,
        }))}
      />
    </Form.Item>
  );
}

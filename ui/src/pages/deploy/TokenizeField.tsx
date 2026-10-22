import { Checkbox, Form, Space } from "antd";
import { useTranslation } from "react-i18next";
import { TokenizeHelp } from "../../components/DeploymentTokenize";

// TokenizeBox is tokenize_enabled's control. Unstated, the server opens the
// routes, so the box shows them on.
function TokenizeBox({
  value,
  onChange,
}: {
  value?: boolean;
  onChange?: (value: boolean) => void;
}) {
  const { t } = useTranslation("common");
  return (
    <Checkbox
      checked={value ?? true}
      onChange={(e) => onChange?.(e.target.checked)}
    >
      {t("tokenize.enable")}
    </Checkbox>
  );
}

// TokenizeField is the deployment's tokenize switch. It is offered for every
// engine, with a note that not every engine serves tokenize: the gateway then
// forwards to a path the engine answers with its own error.
export default function TokenizeField() {
  const { t } = useTranslation("common");
  return (
    <Form.Item
      label={
        <Space size={4}>
          {t("tokenize.title")}
          <TokenizeHelp />
        </Space>
      }
      extra={t("tokenize.otherEngine")}
    >
      <Form.Item name="tokenize_enabled" noStyle>
        <TokenizeBox />
      </Form.Item>
    </Form.Item>
  );
}

import { InfoCircleOutlined } from "@ant-design/icons";
import { Tooltip } from "antd";
import { useTranslation } from "react-i18next";
import { colors } from "../theme/tokens";

// A deployment's tokenize switch: whether the gateway forwards /v1/tokenize
// and /v1/detokenize to its engine (docs/reference/openai-gateway.md). The
// deploy form and the deployment detail show it the same way.

// TokenizeHelp is the question mark that says what the switch does.
export function TokenizeHelp() {
  const { t } = useTranslation("common");
  return (
    <Tooltip
      title={<div style={{ maxWidth: 360 }}>{t("tokenize.help")}</div>}
      trigger={["hover", "focus"]}
    >
      <InfoCircleOutlined
        role="img"
        aria-label={t("tokenize.helpLabel")}
        title={t("tokenize.helpLabel")}
        tabIndex={0}
        style={{
          color: colors.muted,
          fontSize: 13,
          marginLeft: 6,
          cursor: "help",
        }}
      />
    </Tooltip>
  );
}

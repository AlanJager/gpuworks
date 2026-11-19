import { useTranslation } from "react-i18next";
import { HelpTip } from "../ui-kit/HelpTip";

// A deployment's tokenize switch: whether the gateway forwards /v1/tokenize
// and /v1/detokenize to its engine (docs/reference/openai-gateway.md). The
// deploy form and the deployment detail show it the same way.

// TokenizeHelp is the question mark that says what the switch does.
export function TokenizeHelp() {
  const { t } = useTranslation("common");
  return <HelpTip className="ml-1.5" label={t("tokenize.helpLabel")} content={t("tokenize.help")} />;
}

import { useTranslation } from "react-i18next";
import { localizedText } from "../i18n/localizedText";
import type { RoutingPolicyOption } from "../api";
import { HelpTip } from "../ui-kit/HelpTip";

// The server owns the policy list and its localized descriptions. The UI only
// lays out those values behind the shared muted information marker.
export default function RoutingPolicyHelp({ policies }: { policies: RoutingPolicyOption[] }) {
  const { t, i18n } = useTranslation("common");
  const help = policies.length ? (
    <div>
      {policies.map((policy) => (
        <div key={policy.value}>
          <strong>{localizedText(policy.label, i18n.language)}</strong>: {localizedText(policy.description, i18n.language)}
        </div>
      ))}
    </div>
  ) : t("routingPolicy.loading");
  return <HelpTip className="ml-1.5" label={t("routingPolicy.helpLabel")} content={help} />;
}

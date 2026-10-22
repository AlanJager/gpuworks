import { InfoCircleOutlined } from "@ant-design/icons";
import { Tooltip } from "antd";
import { useTranslation } from "react-i18next";
import { localizedText } from "../i18n/localizedText";
import type { RoutingPolicyOption } from "../api";
import { colors } from "../theme/tokens";

// The server owns the policy list and its localized descriptions. The UI only
// lays out those values behind the shared muted information marker.
export default function RoutingPolicyHelp({ policies }: { policies: RoutingPolicyOption[] }) {
  const { t, i18n } = useTranslation("common");
  const help = policies.length ? (
    <div style={{ maxWidth: 380 }}>
      {policies.map((policy) => (
        <div key={policy.value}>
          <strong>{localizedText(policy.label, i18n.language)}</strong>: {localizedText(policy.description, i18n.language)}
        </div>
      ))}
    </div>
  ) : t("routingPolicy.loading");
  return (
    <Tooltip title={help} trigger={["hover", "focus"]}>
      <InfoCircleOutlined
        role="img"
        aria-label={t("routingPolicy.helpLabel")}
        title={t("routingPolicy.helpLabel")}
        tabIndex={0}
        style={{ color: colors.muted, fontSize: 13, marginLeft: 6, cursor: "help" }}
      />
    </Tooltip>
  );
}

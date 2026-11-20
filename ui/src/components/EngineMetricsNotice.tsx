import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Alert } from "../pages/DeploymentControls";

// What the deployment panel says when the server's signal sources show the
// engine gave it nothing to draw (ADR 0039). The panel itself already draws
// whatever the gateway measured; this only explains where the numbers come from.
//   gatewayOnly: every signal with data came from the gateway.
//   noTraffic:   no signal has data from either source in the window.
export default function EngineMetricsNotice({ deploymentId, kind }: { deploymentId: number; kind: "gatewayOnly" | "noTraffic" }) {
  const { t } = useTranslation("deployments");
  const toServiceMonitor = <Link to={`/monitor/d/${deploymentId}`}>{t("metrics.openServiceMonitor")}</Link>;
  const [message, description] = kind === "gatewayOnly"
    ? [t("metrics.engineReportsNone"), t("metrics.engineReportsNoneDescription")]
    : [t("metrics.noEngineNoTraffic"), t("metrics.noEngineNoTrafficDescription")];
  return (
    <Alert
      type="info"
      showIcon
      message={message}
      description={<>{description} {toServiceMonitor}</>}
      style={{ marginBottom: 12 }}
    />
  );
}

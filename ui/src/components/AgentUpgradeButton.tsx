import { Button } from "antd";
import { useTranslation } from "react-i18next";
import type { UpgradePhase } from "../agentUpgrade";

export default function AgentUpgradeButton({
  online,
  phase,
  onUpgrade,
}: {
  online: boolean;
  phase?: UpgradePhase;
  onUpgrade: () => void;
}) {
  const { t } = useTranslation("nodes");
  return (
    <Button
      size="small"
      type="primary"
      disabled={!online || phase === "waiting"}
      loading={!!phase}
      onClick={onUpgrade}
    >
      {t(phase === "waiting" ? "upgrade.waitingReconnect" : "upgrade.action")}
    </Button>
  );
}

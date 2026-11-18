import { Button } from "antd";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import type { UpgradePhase } from "../agentUpgrade";

export default function AgentUpgradeButton({
  nodeUUID, online, eligibility, phase, onUpgrade,
}: {
  nodeUUID: string;
  online: boolean;
  eligibility?: string;
  phase?: UpgradePhase;
  onUpgrade: () => void;
}) {
  const { t } = useTranslation("nodes");
  const [checked, setChecked] = useState<string>();
  const [checking, setChecking] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const generation = useRef(0);
  const active = useRef(false);
  useEffect(() => {
    generation.current++;
    active.current = false;
    setChecked(undefined); setChecking(false); setError(""); setNotice("");
    return () => { generation.current++; };
  }, [nodeUUID, eligibility, online, phase]);
  const status = checked ?? eligibility;
  const unavailable = !!status && status !== "eligible";
  const upgrade = async () => {
    if (active.current || !online || phase || unavailable) return;
    active.current = true; setChecking(true); setError(""); setNotice("");
    const current = generation.current;
    try {
      // Eligibility and build identity belong to the server. Recheck the
      // selected node because it may have reconnected since the drawer read.
      const preview = await api.agentUpgradePreview({ node_uuids: [nodeUUID] });
      if (current !== generation.current) return;
      const candidate = preview.nodes.find((node) => node.node_uuid === nodeUUID);
      const next = candidate?.eligibility ?? "unavailable";
      // A preview rejection can recover while this node's facts stay the
      // same (another node releases a batch, or an artifact becomes ready).
      setChecked(next === "already_current" ? next : undefined);
      if (preview.blocked_by_item) {
        const blocker = preview.blocked_by_item;
        setNotice(t(blocker.reason === "unconfirmed" ? "batchUpgrade.blocked" : "batchUpgrade.activeBlock", { name: blocker.name || blocker.node_uuid }));
      } else if (next !== "eligible" && next !== "already_current") {
        setNotice(t(`batchUpgrade.reason.${candidate?.reason || next}`, {
          defaultValue: candidate?.reason || t(`batchUpgrade.reason.${next}`, { defaultValue: t("batchUpgrade.eligibility.unavailable") }),
        }));
      }
      if (next === "eligible" && !preview.blocked_by_item) onUpgrade();
    } catch (e) {
      if (current === generation.current) setError((e as Error).message);
    } finally {
      if (current === generation.current) { active.current = false; setChecking(false); }
    }
  };
  return <>
    <Button size="small" type="primary"
      disabled={!online || !!phase || checking || unavailable}
      loading={!!phase || checking}
      title={unavailable ? t(`batchUpgrade.reason.${status}`, { defaultValue: t("batchUpgrade.eligibility.unavailable") }) : undefined}
      onClick={() => void upgrade()}>
      {t(phase === "waiting" ? "upgrade.waitingReconnect" : status === "already_current" ? "batchUpgrade.eligibility.already_current" : "upgrade.action")}
    </Button>
    {notice && <span role="status">{notice}</span>}
    {error && <span role="alert">{t("upgrade.requestFailed", { detail: error })}</span>}
  </>;
}

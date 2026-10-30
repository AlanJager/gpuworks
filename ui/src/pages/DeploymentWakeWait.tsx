import { Alert, Button, Space, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { formatTime } from "../i18n/datetime";
import { durationText } from "./durationText";

// WakeRefusalState is why the last wake was refused: the server's own words,
// and who holds the memory. A card held by a process outside the platform
// cannot be freed by any order of ours.
export interface WakeRefusalState {
  code: "WAKE_INSUFFICIENT_GPU_MEMORY" | "WAKE_GPU_HELD_OUTSIDE";
  text: string;
  occupants: string[];
}

// occupantNames reads the occupants a refusal names: the server sends them as a
// list of deployment names.
export function occupantNames(value: unknown): string[] {
  if (Array.isArray(value)) return value.filter((item): item is string => typeof item === "string" && item !== "");
  return typeof value === "string" && value ? [value] : [];
}

// WakeRefusalAlert shows a refused wake. When the cards are short because
// other deployments hold them, it offers the two ways on: wait for room (the
// server wakes the sleepers once the cards are free) or put the occupants to
// sleep and then wake.
export function WakeRefusalAlert({ refusal, limitSec, busy, onWait, onYield }: {
  refusal: WakeRefusalState;
  // How long a waiting wake waits for room, as the server says.
  limitSec: number;
  busy: boolean;
  onWait: () => void;
  onYield: () => void;
}) {
  const { t } = useTranslation("deployments");
  if (refusal.code === "WAKE_GPU_HELD_OUTSIDE") {
    return <Alert type="warning" showIcon style={{ flexBasis: "100%" }} message={t("sleep.refused.heldOutside")} description={refusal.text} />;
  }
  const occupants = refusal.occupants.length > 0 ? refusal.occupants.join(", ") : t("sleep.refused.theOccupants");
  return (
    <Alert
      type="warning"
      showIcon
      style={{ flexBasis: "100%" }}
      message={t("sleep.refused.title")}
      description={
        <Space direction="vertical" size={8}>
          <Typography.Text>{refusal.text}</Typography.Text>
          <Space wrap>
            <Button size="small" loading={busy} onClick={onWait}>{t("sleep.refused.wait")}</Button>
            <Button size="small" type="primary" loading={busy} onClick={onYield}>{t("sleep.refused.yield", { occupants })}</Button>
          </Space>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>{t("sleep.refused.hint", { limit: durationText(t, limitSec) })}</Typography.Text>
        </Space>
      }
    />
  );
}

// WakeWaitingBanner says a wake of this deployment waits for room since when,
// and lets the operator give up waiting.
export function WakeWaitingBanner({ since, limitSec, busy, onCancel }: { since: string; limitSec: number; busy: boolean; onCancel: () => void }) {
  const { t } = useTranslation("deployments");
  return (
    <Alert
      type="info"
      showIcon
      style={{ flexBasis: "100%" }}
      message={t("sleep.waiting.since", { time: formatTime(since) })}
      description={t("sleep.waiting.hint", { limit: durationText(t, limitSec) })}
      action={<Button size="small" loading={busy} onClick={onCancel}>{t("sleep.waiting.cancel")}</Button>}
    />
  );
}

import type { MonitorService } from "../../api";

export const STATE_BADGE: Record<MonitorService["state"], "success" | "info" | "neutral" | "danger" | "warning"> = {
  running: "success",
  offline: "warning",
  starting: "info",
  stopped: "neutral",
  failed: "danger",
  asleep: "warning",
};

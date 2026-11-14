import type { MonitorService } from "../../api";

export const STATE_BADGE: Record<MonitorService["state"], "success" | "info" | "neutral" | "danger" | "warning"> = {
  running: "success",
  offline: "warning",
  starting: "info",
  stopped: "neutral",
  failed: "danger",
  asleep: "warning",
};

// STATE_ORDER lists service states serving first, then the ones that
// need a look, then the deliberately stopped.
export const STATE_ORDER: MonitorService["state"][] = ["running", "starting", "asleep", "offline", "failed", "stopped"];

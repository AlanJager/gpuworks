// Shared machine-state presentation. Human-readable text always comes from the
// caller's active i18n instance so a language switch updates existing views.
export type BadgeStatus = "success" | "default" | "error" | "warning" | "processing";
type Translate = (key: string) => string;

const table: Record<string, { status: BadgeStatus; key: string }> = {
	active: { status: "processing", key: "states.active" },
  pending: { status: "warning", key: "states.pending" },
  pulling: { status: "processing", key: "states.pulling" },
  starting: { status: "processing", key: "states.starting" },
  optimizing: { status: "processing", key: "states.optimizing" },
  "pending-removal": { status: "warning", key: "states.pendingRemoval" },
  draining: { status: "warning", key: "states.draining" },
  removing: { status: "warning", key: "states.removing" },
  deleting: { status: "processing", key: "states.deleting" },
  running: { status: "success", key: "states.running" },
  stopped: { status: "default", key: "states.stopped" },
  failed: { status: "error", key: "states.failed" },
};

export function stateLabel(s: string, t: Translate): { status: BadgeStatus; text: string } {
  const visual = table[s];
  return visual ? { status: visual.status, text: t(visual.key) } : { status: "default", text: s };
}

export function effectiveDeploymentState(deployment: { runtime_state: string; desired_state?: string }): string {
	// A delete in flight outranks whatever the runtime reports: the engine may
	// still be running or already gone, but the deployment is going away, and
	// when a node never confirms teardown the row stays here — exactly the
	// state an operator has to see instead of a plain running/stopped badge.
	if (deployment.desired_state === "deleting") return "deleting";
	return deployment.runtime_state;
}

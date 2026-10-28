import type { ReactNode } from "react";
import type { TFunction } from "i18next";
import type { Deployment } from "../api";
import { formatTime } from "../i18n/datetime";
import IdleSleepEditor from "./DeploymentIdleSleep";
import NodeReplacementEditor, { nodeReplacementSeconds } from "./DeploymentNodeReplacement";
import ServiceLevelEditor, { canShareGPUs, levelOf } from "./DeploymentServiceLevel";
import ScheduleSection, { scheduleOrderText, type Schedules } from "./DeploymentSchedule";
import { durationText } from "./nodeReplacementEvents";

// What a section reads and calls: the deployment, the schedule rows the card
// loaded, and the reload after a change.
export interface PolicyContext {
  dep: Deployment;
  schedules: Schedules;
  onChanged: () => void;
}

// A PolicySection is one kind of standing policy on a deployment: its name,
// whether the deployment has it, the phrase it adds to the card's summary and
// the editor that changes it. The card lists these in order, so a new policy
// is one more entry here.
export interface PolicySection {
  key: string;
  // The i18n key of its name, in the deployments namespace.
  label: string;
  visible: (dep: Deployment) => boolean;
  summary: (t: TFunction<"deployments">, context: PolicyContext) => string;
  render: (context: PolicyContext) => ReactNode;
}

const idleAfterSec = (dep: Deployment) => dep.idle_sleep?.after_sec ?? 0;

export const policySections: PolicySection[] = [
  {
    key: "failover",
    label: "nodeReplacement.title",
    visible: () => true,
    summary: (t, { dep }) => {
      const sec = nodeReplacementSeconds(dep);
      return sec > 0 ? durationText(t, sec) : t("schedulingPolicy.off");
    },
    render: ({ dep, onChanged }) => (
      <NodeReplacementEditor key={`${dep.id}-${nodeReplacementSeconds(dep)}`} dep={dep} onChanged={onChanged} />
    ),
  },
  {
    key: "schedule",
    label: "automation.schedule.title",
    visible: () => true,
    summary: (t, { schedules }) => scheduleSummary(t, schedules),
    render: ({ dep, schedules }) => <ScheduleSection dep={dep} schedules={schedules} />,
  },
  {
    key: "idle",
    label: "automation.idle.title",
    // A deployment that cannot sleep has no idle sleep to set; one that kept a
    // setting from when it could still shows it so it can be turned off.
    visible: (dep) => dep.sleepable || idleAfterSec(dep) > 0,
    summary: (t, { dep }) => {
      const sec = idleAfterSec(dep);
      return sec > 0
        ? t("schedulingPolicy.idleOn", { minutes: Math.round(sec / 60), level: dep.idle_sleep?.level ?? 2 })
        : t("schedulingPolicy.off");
    },
    render: ({ dep, onChanged }) => (
      <IdleSleepEditor key={`idle-${dep.id}-${idleAfterSec(dep)}-${dep.idle_sleep?.level ?? 0}`} dep={dep} onChanged={onChanged} />
    ),
  },
  {
    key: "serviceLevel",
    label: "serviceLevel.title",
    // Only a sleepable single-node deployment can leave resident; one that
    // kept another level from when it could still shows it so it can go back.
    visible: (dep) => canShareGPUs(dep) || levelOf(dep) !== "resident",
    summary: (t, { dep }) => t(`serviceLevel.name.${levelOf(dep)}`),
    render: ({ dep, onChanged }) => <ServiceLevelEditor key={`level-${dep.id}-${levelOf(dep)}`} dep={dep} onChanged={onChanged} />,
  },
];

// scheduleSummary counts the orders and names the next one to run among the
// enabled ones.
function scheduleSummary(t: TFunction<"deployments">, { rows, error }: Schedules): string {
  if (error) return t("schedulingPolicy.loadFailed");
  if (rows === null) return t("schedulingPolicy.loading");
  if (rows.length === 0) return t("schedulingPolicy.none");
  const count = t("schedulingPolicy.scheduleCount", { count: rows.length });
  const upcoming = rows
    .filter((row) => row.enabled && row.next_at)
    .sort((left, right) => new Date(left.next_at!).getTime() - new Date(right.next_at!).getTime());
  if (upcoming.length === 0) return count + t("schedulingPolicy.scheduleAllOff");
  return count + t("schedulingPolicy.scheduleNext", { time: formatTime(upcoming[0].next_at), order: scheduleOrderText(t, upcoming[0]) });
}

// policySummary is the one line the collapsed card shows: each visible
// section's name and what it is set to.
export function policySummary(t: TFunction<"deployments">, context: PolicyContext): string {
  return policySections
    .filter((section) => section.visible(context.dep))
    .map((section) => t("schedulingPolicy.item", { label: t(section.label), value: section.summary(t, context) }))
    .join(t("schedulingPolicy.separator"));
}

// Client for a deployment's standing schedule (/api/v1/deployments/{id}/schedule):
// sleep, wake or a replica count at a time on some weekdays.
import { type List, getJSON, sendJSON } from "./api";

export type ScheduleAction = "sleep" | "wake" | "replicas";

// LifecycleSchedule is one standing order of a deployment: at time (HH:MM) on
// weekdays (0 Sunday to 6 Saturday) in timezone ("" is the server's zone).
export interface LifecycleSchedule {
  id: number;
  action: ScheduleAction;
  weekdays: number[];
  time: string;
  timezone: string;
  sleep_level?: 1 | 2;
  // replicas is stated for a replicas order (0 included, for a PD role the mode
  // lets be empty); role is the PD role it scales, absent otherwise.
  replicas?: number;
  role?: string;
  enabled: boolean;
  next_at?: string;
}

export interface LifecycleScheduleRequest {
  action: ScheduleAction;
  weekdays: number[];
  time: string;
  timezone: string;
  sleep_level?: 1 | 2;
  replicas?: number;
  // role names the PD role a replicas order scales: required for a PD
  // deployment, refused for any other.
  role?: string;
  enabled?: boolean;
}

export const deploymentScheduleApi = {
  list: (id: number) => getJSON<List<LifecycleSchedule>>(`/api/v1/deployments/${id}/schedule`),
  create: (id: number, body: LifecycleScheduleRequest) =>
    sendJSON<LifecycleSchedule>("POST", `/api/v1/deployments/${id}/schedule`, body),
  update: (id: number, scheduleID: number, body: LifecycleScheduleRequest) =>
    sendJSON<LifecycleSchedule>("PUT", `/api/v1/deployments/${id}/schedule/${scheduleID}`, body),
  delete: (id: number, scheduleID: number) =>
    sendJSON<void>("DELETE", `/api/v1/deployments/${id}/schedule/${scheduleID}`),
};

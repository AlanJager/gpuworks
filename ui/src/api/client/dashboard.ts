import { getJSON } from "../http";
import type { DashboardSummary } from "../dashboard";

export const dashboardApi = {
  dashboardSummary: (labelKey?: string) =>
    getJSON<DashboardSummary>(`/api/v1/dashboard/summary${labelKey ? `?label_key=${encodeURIComponent(labelKey)}` : ""}`),
};

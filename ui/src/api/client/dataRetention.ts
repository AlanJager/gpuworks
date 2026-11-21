import { getJSON, sendJSON } from "../http";
import type { DataRetention, DataRetentionStatus, DataRetentionView, ExperienceStatus } from "../dataRetention";

export const dataRetentionApi = {
  dataRetention: () => getJSON<DataRetentionView>("/api/v1/data-retention"),
  experience: () => getJSON<ExperienceStatus>("/api/v1/experience"),
  setExperienceEnabled: (enabled: boolean) => sendJSON<ExperienceStatus>("PUT", "/api/v1/experience/enabled", { enabled }),
  resetExperience: () => sendJSON<ExperienceStatus>("POST", "/api/v1/experience/reset"),
  rotateExperiencePassword: () => sendJSON<ExperienceStatus>("POST", "/api/v1/experience/password"),
  putDataRetention: (value: DataRetention) => sendJSON<DataRetentionView>("PUT", "/api/v1/data-retention", value),
  dataRetentionStatus: () => getJSON<DataRetentionStatus>("/api/v1/data-retention/status"),
};

export interface SchedulePlan {
  gpus: number[];
  tp: number;
  per_gpu_bytes: number;
  score: number;
  reasons: ScheduleReason[];
}

export interface ScheduleMessage {
  code?: string;
  params?: Record<string, unknown>;
  message?: string;
}

export type ScheduleMessageLike = ScheduleMessage | string;

export interface ScheduleReason extends ScheduleMessage {
  score?: number;
}

export interface ScheduleRejected {
  gpus?: number[];
  tp: number;
  reason: ScheduleMessageLike;
}

export interface ScheduleExplain {
  chosen?: SchedulePlan | null;
  rejected?: ScheduleRejected[];
  topo_highlight?: {
    gpus?: number[];
    links?: [number, number][];
    numa?: number[];
  };
  filters?: ScheduleReason[];
}
export interface ProductWarning {
  code: string;
  params?: Record<string, unknown>;
}

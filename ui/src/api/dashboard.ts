export interface CapacityGroup {
  key: string;
  unlabeled?: boolean;
  nodes: number;
  gpus: number;
  vram_gib: number;
  unusable_gpus: number;
}

/** The fleet's cards counted once each, in the scheduling overview's states. */
export interface SchedulingCardSummary {
  nodes: number;
  online_nodes: number;
  cards: number;
  awake: number;
  asleep: number;
  free: number;
  held_outside: number;
  unhealthy: number;
  offline: number;
  contention: { waiting_wakes: number; held_requests: number; yields_last_hour: number };
}

export interface DashboardSummary {
  current_metrics?: {
    utilization_pct: number | null;
    temperature_c: number | null;
    power_watts: number | null;
    memory_used_bytes: number | null;
    /** VRAM used and owned by the online nodes' cards, summed over the cards with both samples fresh; null when none has. */
    vram_used_bytes?: number | null;
    vram_total_bytes?: number | null;
  };
  nodes: {
    total: number;
    online: number;
    offline: number;
    connecting: number;
    gpus: number;
    unusable_gpus: number;
    vram_gib: number;
    memory_gib: number;
  };
  /** Every deployment falls in exactly one bucket; other is the ones in no named bucket. */
  deployments: { total: number; running: number; warming: number; sleeping?: number; pending?: number; other?: number; sleepable: number };
  /** The scheduling overview's card summary; null when it cannot be built. */
  /** allocated = cards a deployment holds (awake + asleep), counted by the server. */
  cards?: (SchedulingCardSummary & { allocated: number }) | null;
  tasks?: { batches: { active: number; waiting: number } | null; benchmarks: number };
  by_vendor: CapacityGroup[];
  label_keys: string[];
  label_key: string;
  by_label: CapacityGroup[];
}

import type { CheckStatus } from "../checkStatus";

/** The mount a doctor path lives on; absent from agents that predate it. */
export interface DoctorMount {
  point: string;
  fs_type: string;
  source?: string;
  options?: string;
  network?: boolean;
  noexec?: boolean;
  read_only?: boolean;
  ephemeral?: boolean;
}

export interface DoctorDisk {
  kind: string;
  path: string;
  total_bytes: number;
  avail_bytes: number;
  mount?: DoctorMount;
}

export interface DoctorField {
  name: string;
  value: string;
}

export interface DoctorTarget {
  index: number;
  pcie_bdf?: string;
  evidence?: DoctorField[];
  probe?: string;
  recheck?: string;
  actions?: string[];
  processes?: { pid: number; name: string }[];
}

/** A doctor check that failed, warned or did not run (skip, with why in code). */
export interface DoctorFinding {
  id?: string;
  status: "fail" | "warn" | "skip";
  code: string;
  targets?: DoctorTarget[];
  disk?: DoctorDisk;
  probe?: string;
  evidence?: DoctorField[];
  recheck?: string;
}

/** A check in the Server's unified node diagnosis. */
export interface NodeDoctorCheck {
  stage: string;
  status: CheckStatus;
  code: string;
  vantage: string;
  target?: string;
  source?: string;
  observed_at?: string;
  finding?: DoctorFinding;
}

export interface NodeDoctorReport {
  checked_at?: string;
  summary?: CheckStatus;
  checks?: NodeDoctorCheck[];
  report: string;
  host?: {
    hostname: string;
    os: string;
    arch: string;
    kernel: string;
    cpu_model: string;
    cpu_cores: number;
    mem_total_bytes: number;
    mem_avail_bytes: number;
    disks: DoctorDisk[];
  };
  runtimes?: { name: string; status: "ok" | "not_found" | "query_failed"; version?: string; selected?: boolean; command: string }[];
  accelerators?: {
    vendor: string;
    driver?: string;
    gpus: { index: number; name: string; memory_bytes: number; pcie_bdf?: string; numa_node: number; health_status: string; health_code?: string }[];
  };
  findings?: DoctorFinding[];
  facts?: DoctorFact[];
}

/** Whom a runtime's workloads run as; set on a runtime_identity fact. */
export interface DoctorRunIdentity {
  runtime: string;
  pending?: boolean;
  /** The dedicated account; absent means the agent's own user. */
  user?: string;
  uid: number;
  /** Why the dedicated account is not used, as the system reported it. */
  fallback?: string;
}

/**
 * A statement without a verdict. value is the agent's English wording; a fact
 * the console words itself carries the statement typed too.
 */
export interface DoctorFact {
  code: string;
  value: string;
  identity?: DoctorRunIdentity;
  /** The paths a network_filesystem fact covers, all on one mount. */
  disks?: DoctorDisk[];
}

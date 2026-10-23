import type { Granularity } from "../../analyticsApi";

/** What the page hands every tab: the resolved query, and the URL's other parameters to read and write. */
export interface TabProps {
  from: number;
  to: number;
  granularity: Granularity;
  deployment?: number;
  /** The statistics time zone buckets and days are read in. */
  tz: string;
  /** Changes when the user asks for a refresh, to rerun an unchanged query. */
  reload: number;
  search: URLSearchParams;
  /** Sets (or with undefined removes) URL parameters, keeping the others. */
  setSearch: (patch: Record<string, string | string[] | undefined>) => void;
}

export const TAB_KEYS = ["usage", "quality", "gpus", "gpu-time"] as const;
export type TabKey = (typeof TAB_KEYS)[number];

export function isTabKey(value: string | undefined): value is TabKey {
  return !!value && (TAB_KEYS as readonly string[]).includes(value);
}

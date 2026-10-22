import { createContext, useContext, useMemo, type ReactNode } from "react";
import { useSearchParams } from "react-router-dom";
import { Segmented, Space, Switch, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { MonitorRange } from "../../api";

// The monitor pages share one time range + "follow latest" toggle, carried in
// the URL (?range=&follow=) so a link or a refresh keeps the same view (plan
// §1: "共享 URL 承载的时间范围...与'跟随最新'开关").
export const RANGE_OPTIONS: MonitorRange[] = ["15m", "1h", "6h", "24h", "168h"];
const DEFAULT_RANGE: MonitorRange = "1h";

function isRange(value: string | null): value is MonitorRange {
  return !!value && (RANGE_OPTIONS as string[]).includes(value);
}

interface TimeRangeState {
  range: MonitorRange;
  follow: boolean;
  setRange: (range: MonitorRange) => void;
  setFollow: (follow: boolean) => void;
}

const TimeRangeCtx = createContext<TimeRangeState | null>(null);

export function TimeRangeProvider({ children }: { children: ReactNode }) {
  const [searchParams, setSearchParams] = useSearchParams();
  const rangeParam = searchParams.get("range");
  const followParam = searchParams.get("follow");
  const range: MonitorRange = isRange(rangeParam) ? rangeParam : DEFAULT_RANGE;
  // Follow defaults on: a monitor page that does not refresh itself defeats
  // its own purpose. ?follow=0 (or =false) is the only way to turn it off.
  const follow = followParam !== "0" && followParam !== "false";

  const setRange = (next: MonitorRange) => {
    const params = new URLSearchParams(searchParams);
    params.set("range", next);
    setSearchParams(params, { replace: true });
  };
  const setFollow = (next: boolean) => {
    const params = new URLSearchParams(searchParams);
    params.set("follow", next ? "1" : "0");
    setSearchParams(params, { replace: true });
  };

  const value = useMemo(
    () => ({ range, follow, setRange, setFollow }),
    [range, follow, searchParams],
  );
  return <TimeRangeCtx.Provider value={value}>{children}</TimeRangeCtx.Provider>;
}

export function useTimeRange(): TimeRangeState {
  const ctx = useContext(TimeRangeCtx);
  if (!ctx) throw new Error("useTimeRange must be used within a TimeRangeProvider");
  return ctx;
}

// TimeRangeControl is the header widget: a range Segmented plus the follow
// Switch, both writing straight through to the URL via useTimeRange.
export default function TimeRangeControl() {
  const { t } = useTranslation("monitoring");
  const { range, follow, setRange, setFollow } = useTimeRange();
  return (
    <Space size={12} align="center">
      <Segmented
        size="small"
        value={range}
        onChange={(value) => setRange(value as MonitorRange)}
        options={RANGE_OPTIONS.map((value) => ({ label: t(`range.${value}`), value }))}
      />
      <Space size={6} align="center">
        <Switch size="small" checked={follow} onChange={setFollow} aria-label={t("range.follow")} />
        <Typography.Text style={{ fontSize: 12 }}>{t("range.follow")}</Typography.Text>
      </Space>
    </Space>
  );
}

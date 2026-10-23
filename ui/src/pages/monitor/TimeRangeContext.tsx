import { createContext, useContext, useMemo, type ReactNode } from "react";
import { useSearchParams } from "react-router-dom";
import { DatePicker, Segmented, Space, Switch, Tooltip, Typography } from "antd";
import dayjs, { type Dayjs } from "dayjs";
import { useTranslation } from "react-i18next";
import type { MonitorRange } from "../../api";
import { MONITOR_PRESETS, customRange, isPreset, pickedBounds, type MonitorPreset } from "../../monitorRange";
import { formatTime } from "../../i18n/datetime";

// The monitor pages share one time range + "follow latest" toggle, carried in
// the URL (?range=&follow=, or ?from=&to= for a range the operator picked) so
// a link or a refresh keeps the same view (plan §1: "共享 URL 承载的时间范围...与'跟随最新'开关").
export const RANGE_OPTIONS: MonitorPreset[] = MONITOR_PRESETS;
const DEFAULT_RANGE: MonitorPreset = "1h";
// The server keeps raw samples at most 30 days (data retention bounds).
const PICK_BACK_DAYS = 30;

interface TimeRangeState {
  range: MonitorRange;
  /** picked is the window of a range the operator picked, null for a preset. */
  picked: { from: number; to: number } | null;
  /** follow polls for new data; a picked range never does. */
  follow: boolean;
  setRange: (range: MonitorPreset) => void;
  setPicked: (from: number, to: number) => void;
  setFollow: (follow: boolean) => void;
}

const TimeRangeCtx = createContext<TimeRangeState | null>(null);

export function TimeRangeProvider({ children }: { children: ReactNode }) {
  const [searchParams, setSearchParams] = useSearchParams();
  const rangeParam = searchParams.get("range");
  const followParam = searchParams.get("follow");
  const pickedParam = pickedBounds(`${searchParams.get("from") ?? ""}~${searchParams.get("to") ?? ""}`);
  const range: MonitorRange = pickedParam
    ? customRange(pickedParam.from, pickedParam.to)
    : isPreset(rangeParam) ? rangeParam : DEFAULT_RANGE;
  // Follow defaults on: a monitor page that does not refresh itself defeats
  // its own purpose. ?follow=0 (or =false) is the only way to turn it off;
  // a picked range is in the past and has nothing to follow.
  const follow = !pickedParam && followParam !== "0" && followParam !== "false";

  const setRange = (next: MonitorPreset) => {
    const params = new URLSearchParams(searchParams);
    params.set("range", next);
    params.delete("from");
    params.delete("to");
    setSearchParams(params, { replace: true });
  };
  const setPicked = (from: number, to: number) => {
    const params = new URLSearchParams(searchParams);
    params.delete("range");
    params.set("from", String(Math.round(from)));
    params.set("to", String(Math.round(to)));
    setSearchParams(params, { replace: true });
  };
  const setFollow = (next: boolean) => {
    const params = new URLSearchParams(searchParams);
    params.set("follow", next ? "1" : "0");
    setSearchParams(params, { replace: true });
  };

  const value = useMemo(
    () => ({ range, picked: pickedParam, follow, setRange, setPicked, setFollow }),
    [range, follow, searchParams],
  );
  return <TimeRangeCtx.Provider value={value}>{children}</TimeRangeCtx.Provider>;
}

export function useTimeRange(): TimeRangeState {
  const ctx = useContext(TimeRangeCtx);
  if (!ctx) throw new Error("useTimeRange must be used within a TimeRangeProvider");
  return ctx;
}

/** outsidePickable is a day the picker refuses: in the future, or before raw samples are kept. */
export function outsidePickable(day: Dayjs, now = dayjs()): boolean {
  return day.isAfter(now.endOf("day")) || day.isBefore(now.subtract(PICK_BACK_DAYS, "day").startOf("day"));
}

// TimeRangeControl is the header widget: the presets, a picker for any other
// range within the raw retention, and the follow Switch, all writing straight
// through to the URL via useTimeRange.
export default function TimeRangeControl() {
  const { t } = useTranslation("monitoring");
  const { range, picked, follow, setRange, setPicked, setFollow } = useTimeRange();
  return (
    <Space size={12} align="center" wrap>
      <Segmented
        size="small"
        value={picked ? "custom" : range}
        onChange={(value) => {
          if (value !== "custom") setRange(value as MonitorPreset);
        }}
        options={[
          ...RANGE_OPTIONS.map((value) => ({ label: t(`range.${value}`), value })),
          ...(picked ? [{ label: t("range.custom"), value: "custom" }] : []),
        ]}
      />
      <DatePicker.RangePicker
        size="small"
        showTime={{ format: "HH:mm" }}
        format="YYYY-MM-DD HH:mm"
        allowClear={false}
        aria-label={t("range.pick")}
        placeholder={[t("range.pickFrom"), t("range.pickTo")]}
        value={picked ? [dayjs(picked.from), dayjs(picked.to)] : null}
        disabledDate={(day) => outsidePickable(day)}
        onChange={(values) => {
          const [from, to] = values ?? [];
          if (from && to && to.valueOf() > from.valueOf()) setPicked(from.valueOf(), Math.min(to.valueOf(), Date.now()));
        }}
      />
      <Tooltip title={picked ? t("range.pickedNoFollow") : undefined}>
        <Space size={6} align="center">
          <Switch size="small" checked={follow} disabled={!!picked} onChange={setFollow} aria-label={t("range.follow")} />
          <Typography.Text style={{ fontSize: 12 }}>{t("range.follow")}</Typography.Text>
        </Space>
      </Tooltip>
      {picked && (
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {t("range.pickedNote", { from: formatTime(new Date(picked.from).toISOString()), to: formatTime(new Date(picked.to).toISOString()) })}
        </Typography.Text>
      )}
    </Space>
  );
}

import { useState } from "react";
import { DatePicker, Select } from "antd";
import type { Dayjs } from "dayjs";
import dayjs from "dayjs";
import { useTranslation } from "react-i18next";
import { RANGE_KEYS, type AbsoluteWindow, type RangeKey } from "./logsPageRange";

interface Props {
  range: RangeKey;
  window: AbsoluteWindow | null;
  onPreset: (range: RangeKey) => void;
  onWindow: (window: AbsoluteWindow) => void;
}

// LogsRangeControl is the log console's time-range control: the presets,
// plus 自定义 revealing an absolute start/end picker. Picking 自定义 only
// reveals the picker; the query keeps its current window until both ends
// are confirmed there. A deep link's from/to arrives as the same custom
// window, so it shows in the picker and can be adjusted.
export default function LogsRangeControl({ range, window, onPreset, onWindow }: Props) {
  const { t } = useTranslation("logs");
  const [drafting, setDrafting] = useState(false);
  const custom = range === "custom";
  return (
    <>
      <Select
        data-testid="range-select"
        aria-label={t("search.rangeLabel")}
        style={{ minWidth: 130 }}
        value={drafting ? "custom" : range}
        onChange={(next: RangeKey) => {
          setDrafting(next === "custom" && !custom);
          if (next !== "custom") onPreset(next);
        }}
        options={[...RANGE_KEYS, "custom" as const].map((r) => ({ value: r, label: t(`range.${r}`) }))}
      />
      {(drafting || custom) && (
        <DatePicker.RangePicker
          id={{ start: "logs-range-start", end: "logs-range-end" }}
          showTime={{ format: "HH:mm:ss" }}
          format="YYYY-MM-DD HH:mm:ss"
          allowClear={false}
          value={custom && window ? [dayjs(window.from), dayjs(window.to)] : null}
          disabledDate={(d: Dayjs) => d.isAfter(dayjs().endOf("day"))}
          onOk={(dates) => {
            const [from, to] = (dates ?? []) as Array<Dayjs | null>;
            if (from && to && to.valueOf() > from.valueOf()) {
              setDrafting(false);
              onWindow({ from: from.valueOf(), to: to.valueOf() });
            }
          }}
        />
      )}
    </>
  );
}

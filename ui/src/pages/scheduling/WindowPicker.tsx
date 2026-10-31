import { Segmented } from "antd";
import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";
import type { TimelineWindowOption } from "../../schedulingApi";

/** windowLabel names a window the server offers by its length. */
export function windowLabel(t: TFunction, option: Pick<TimelineWindowOption, "seconds">): string {
  return option.seconds < 3600
    ? t("scheduling:timeline.minutes", { minutes: Math.round(option.seconds / 60) })
    : t("scheduling:timeline.hours", { hours: Math.round(option.seconds / 3600) });
}

/**
 * WindowPicker offers the windows a timeline answer lists: the server's
 * list, so the page keeps none of its own. Nothing shows before the first
 * answer names them.
 */
export default function WindowPicker({ windows, value, onChange }: { windows: TimelineWindowOption[]; value: string; onChange: (key: string) => void }) {
  const { t } = useTranslation("scheduling");
  if (windows.length === 0) return null;
  return (
    <Segmented<string>
      aria-label={t("timeline.window")}
      value={value}
      options={windows.map((option) => ({ value: option.key, label: windowLabel(t, option) }))}
      onChange={onChange}
    />
  );
}

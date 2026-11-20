import { ClockCircleOutlined, EllipsisOutlined, PauseOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import type { CardHealthCell } from "../../api";
import { agoText } from "../../format";
import { Tooltip } from "../ui/tooltip";
import { checkName, codeText, statusName } from "./text";

const fills: Record<string, string> = {
  pass: "bg-success-strong",
  warn: "bg-warning-strong",
  fail: "bg-danger-strong",
  skip: "bg-emphasis",
  na: "bg-transparent",
};

// CheckCell is one check on one card: a coloured square for what it found,
// grey with an icon for a check that did not run (paused), has not run yet
// (dots) or whose report is too old to say (clock), and a dashed outline for
// one that does not apply. Only a pass is green; stale never is.
export default function CheckCell({
  cell,
  selected,
  onSelect,
}: {
  cell: CardHealthCell;
  selected?: boolean;
  onSelect?: () => void;
}) {
  const { t } = useTranslation("nodes");
  const notRunYet = cell.status === "skip" && cell.code === "CARD_CHECK_NOT_RUN_YET";
  const shown = cell.stale ? "stale" : cell.status;
  const background = cell.stale ? "bg-emphasis" : notRunYet ? "bg-hover" : fills[cell.status] ?? "bg-emphasis";
  const border = cell.status === "na" ? "border border-dashed border-border" : selected ? "border-2 border-brand" : "border border-transparent";
  const icon = cell.stale ? <ClockCircleOutlined /> : notRunYet ? <EllipsisOutlined /> : cell.status === "skip" ? <PauseOutlined /> : null;
  const reason = codeText(t, cell.status, cell.code);
  const ago = cell.checked_at ? agoText(t, (Date.now() - new Date(cell.checked_at).getTime()) / 1000) : "";
  const label = [checkName(t, cell.check), statusName(t, shown), reason, ago && t("cardHealth.lastChecked", { ago })].filter(Boolean).join(" · ");
  return (
    <Tooltip content={label}>
      <button
        type="button"
        aria-label={label}
        aria-pressed={selected}
        data-status={shown}
        disabled={cell.status === "na"}
        onClick={onSelect}
        className={`inline-flex size-6 items-center justify-center rounded p-0 text-label text-fg-muted ${background} ${border} ${cell.status === "na" ? "cursor-default" : "cursor-pointer"}`}
      >
        {icon}
      </button>
    </Tooltip>
  );
}

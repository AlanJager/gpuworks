import { ClockCircleOutlined, EllipsisOutlined, PauseOutlined } from "@ant-design/icons";
import { Tooltip, theme } from "antd";
import { useTranslation } from "react-i18next";
import type { CardHealthCell } from "../../api";
import { agoText } from "../../format";
import { checkName, reasonText, stateName } from "./text";

// CheckCell is one check on one card: a coloured square for what it found,
// grey with an icon for a check that did not run (paused), has not run yet
// (dots) or whose report is too old to say (clock). Stale is never green.
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
  const { token } = theme.useToken();
  const colors: Record<string, string> = {
    pass: token.colorSuccess,
    warn: token.colorWarning,
    fail: token.colorError,
    skip: token.colorFillSecondary,
    pending: token.colorFillQuaternary,
    na: "transparent",
  };
  const shown = cell.stale ? "stale" : cell.state;
  const background = cell.stale ? token.colorFillSecondary : colors[cell.state] ?? token.colorFillSecondary;
  const icon = cell.stale ? <ClockCircleOutlined /> : cell.state === "skip" ? <PauseOutlined /> : cell.state === "pending" ? <EllipsisOutlined /> : null;
  const reason = reasonText(t, cell.state, cell.reason);
  const ago = cell.checked_at ? agoText(t, (Date.now() - new Date(cell.checked_at).getTime()) / 1000) : "";
  const label = [checkName(t, cell.check), stateName(t, shown), reason, ago && t("cardHealth.lastChecked", { ago })].filter(Boolean).join(" · ");
  return (
    <Tooltip title={label}>
      <button
        type="button"
        aria-label={label}
        aria-pressed={selected}
        data-state={shown}
        disabled={cell.state === "na"}
        onClick={onSelect}
        style={{
          width: 24,
          height: 24,
          padding: 0,
          borderRadius: 4,
          cursor: cell.state === "na" ? "default" : "pointer",
          background,
          color: token.colorTextSecondary,
          fontSize: 12,
          display: "inline-flex",
          alignItems: "center",
          justifyContent: "center",
          border: cell.state === "na" ? `1px dashed ${token.colorBorder}` : selected ? `2px solid ${token.colorPrimary}` : "1px solid transparent",
        }}
      >
        {icon}
      </button>
    </Tooltip>
  );
}

import { Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { MonitorSignal } from "../../api";
import { colors } from "../../theme/tokens";
import Sparkline from "./Sparkline";
import { formatSignalValue } from "./signalFormat";

export { formatSignalValue };

// SignalTile is one of the eight cluster-layer signal cards (plan §3.1): a
// big number, an optional denominator, a one-line detail and a sparkline.
// It reports the facts and passes no verdict on them; the one fact it marks
// is fewer replicas ready than wanted. Every tile renders the same four fixed-height rows (label,
// value, detail, sparkline) regardless of state, so the eight tiles in the
// signal row are always the same height - a state with less to show (an
// uncollected tile, a tile with no detail or no sparkline data) gets a
// muted placeholder or an empty slot instead of a shorter tile.
// `collected:false` never renders as a fabricated 0 - it is its own muted
// state, per the wire contract's "Null means not collected" rule.
//
// `detail.no_recent_requests` is a separate, weaker signal: the window has
// samples but nothing happened in the last 5 minutes. For every signal
// except ttft/tpot the server still sends a real value (a rate is simply
// 0, and 0 is not the same claim as "unknown" - an engine driven directly
// on its own port can have waiting=42 while the proxy in front of it is
// quiet), so it is rendered as an annotation in the detail row alongside
// the real number, never as a value suppressor. ttft/tpot are the one
// exception: a quantile has no meaningful "0", so the server omits
// value/denominator entirely and the muted sentence takes the value row's
// place instead.
export default function SignalTile({
  signal,
  label,
  detail,
  unit,
  valueLabel,
  denominatorLabel,
  formatValue = formatSignalValue,
  onClick,
  tooltip,
}: {
  signal: MonitorSignal;
  label: string;
  detail?: string;
  unit?: string;
  // Distinguishes the two numbers of a value/denominator pair for the
  // token_rate tile's detail-row caption ("Input / Output tok/s") - the
  // value row itself stays a bare "a / b" for every paired signal so the
  // number always fits, even at 8 columns.
  valueLabel?: string;
  denominatorLabel?: string;
  formatValue?: (value: number) => string;
  onClick?: () => void;
  // A native title-attribute hint (e.g. ttft/tpot's "this is an upper
  // bound, not a single quantile" caveat) - kept to the plain title
  // attribute rather than a floating tooltip component, matching
  // ReplicaTable's own row-hint pattern.
  tooltip?: string;
}) {
  const { t } = useTranslation("monitoring");
  const short = signal.key === "ready_replicas" && signal.value != null && signal.denominator != null && signal.value < signal.denominator;
  const noRecentRequests = signal.collected && signal.detail?.no_recent_requests === true;
  const hasValue = signal.collected && signal.value != null;
  // Not-collected tiles carry no actionable filter (there is nothing to
  // narrow the table to), so they are not clickable, only informational.
  // no_recent_requests tiles are still collected - clicking still applies
  // whatever filter the signal maps to (which, having no matches, simply
  // clears itself - see MonitorPage's filterForSignal).
  const clickable = signal.collected && !!onClick;
  // token_rate is the only signal with a labeled value/denominator pair;
  // the labels move to the detail row's caption, never the value row.
  const isToken = signal.key === "token_rate" && !!valueLabel && !!denominatorLabel;
  const plainValue = hasValue
    ? signal.denominator != null
      ? `${formatValue(signal.value!)} / ${formatValue(signal.denominator)}`
      : formatValue(signal.value!)
    : null;

  // Detail-row content, in priority order: the no_recent_requests
  // annotation (only meaningful once there is a real value to annotate -
  // ttft/tpot's value-less case already says so in the value row itself),
  // then the token tile's "Input / Output unit" caption, then whatever
  // signal-specific detail text the caller computed (not_ready names,
  // error_rate's 5xx/429/503 breakdown).
  const detailText = noRecentRequests && hasValue
    ? t("signals.noRecentRequests")
    : isToken && signal.collected
      ? `${valueLabel} / ${denominatorLabel}${unit ? ` ${unit}` : ""}`
      : detail;

  // The accessible name always carries the unit (and, for token_rate, the
  // labels) even where the compact 8-column value row visually drops them
  // into the detail row - the aria string is not width-constrained.
  const ariaValuePart = !signal.collected
    ? t("signals.notCollected")
    : !hasValue
      ? t("signals.noRecentRequests")
      : isToken
        ? `${valueLabel} ${formatValue(signal.value!)} / ${denominatorLabel} ${formatValue(signal.denominator!)}${unit ? ` ${unit}` : ""}`
        : `${plainValue}${unit ? ` ${unit}` : ""}`;
  const ariaAnnotation = noRecentRequests && hasValue ? ` (${t("signals.noRecentRequests")})` : "";
  const ariaLabel = `${label}: ${ariaValuePart}${ariaAnnotation}`;

  return (
    <button
      type="button"
      className={`monitor-signal-tile${isToken ? " monitor-signal-tile--token" : ""}`}
      onClick={clickable ? onClick : undefined}
      aria-label={ariaLabel}
      aria-disabled={!signal.collected || undefined}
      title={signal.collected ? tooltip : undefined}
    >
      <span className="monitor-signal-tile__label">
        <Typography.Text type="secondary" className="monitor-signal-tile__label-text">
          {label}
        </Typography.Text>
      </span>

      <span className="monitor-signal-tile__value" data-testid="signal-value-row">
        {!signal.collected ? (
          <Typography.Text type="secondary" className="monitor-signal-tile__muted">
            {t("signals.notCollected")}
          </Typography.Text>
        ) : !hasValue ? (
          <Typography.Text type="secondary" className="monitor-signal-tile__muted">
            {t("signals.noRecentRequests")}
          </Typography.Text>
        ) : (
          <span className="monitor-signal-tile__value-number" style={short ? { color: colors.warningText } : undefined}>
            {plainValue}
            {unit && !isToken && (
              <Typography.Text type="secondary" className="monitor-signal-tile__unit">
                {unit}
              </Typography.Text>
            )}
          </span>
        )}
      </span>

      <span className="monitor-signal-tile__detail" data-testid="signal-detail-row">
        {detailText ? (
          <Typography.Text type="secondary" className="monitor-signal-tile__detail-text">
            {detailText}
          </Typography.Text>
        ) : null}
      </span>

      <span className="monitor-signal-tile__spark" data-testid="signal-spark-slot">
        <Sparkline points={signal.spark} color={colors.primary} />
      </span>
    </button>
  );
}

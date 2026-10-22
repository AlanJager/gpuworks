import { Typography } from "antd";
import { useTranslation } from "react-i18next";
import { formatTimeSec } from "../../i18n/datetime";
import { colors } from "../../theme/tokens";

// Shared "data as of ..." caption for both monitor pages. as_of <= 0 means
// the server has no sample at all yet (a brand new deployment, or a fixture
// with no data) - formatting it as a time would print the 1970 epoch, so
// that state gets its own sentence instead. `stale` gets its own visible
// sentence too (not just a color change), so its meaning survives grayscale
// screenshots, colorblind users and plain-text assertions alike.
export default function AsOfCaption({ asOf, stale }: { asOf: number; stale: boolean }) {
  const { t } = useTranslation("monitoring");
  return (
    <Typography.Text style={{ display: "block", marginBottom: 8, fontSize: 12, color: stale ? colors.warningText : colors.muted }}>
      {asOf > 0 ? t("overview.asOf", { time: formatTimeSec(asOf) }) : t("overview.noSamples")}
      {stale && ` ${t("overview.stale")}`}
    </Typography.Text>
  );
}

import { InfoCircleOutlined } from "@ant-design/icons";
import { Tooltip } from "antd";
import { useTranslation } from "react-i18next";
import { colors } from "../../theme/tokens";

// InfoTitle is a card/chart title followed by an info icon whose hover
// tooltip defines the metric precisely (what is measured, over which
// window, and how the quantiles are combined). The icon carries an
// aria-label naming the title so assistive tech and tests can find each
// explanation without relying on the icon glyph.
export default function InfoTitle({ title, help }: { title: string; help: string }) {
  const { t } = useTranslation("monitoring");
  return (
    <span style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
      <span>{title}</span>
      <Tooltip title={help} trigger={["hover", "focus"]} styles={{ root: { maxWidth: 420 } }}>
        <InfoCircleOutlined
          aria-label={t("deployment.helpIcon", { title })}
          title={help}
          role="img"
          tabIndex={0}
          style={{ color: colors.muted, fontSize: 13, cursor: "help" }}
        />
      </Tooltip>
    </span>
  );
}

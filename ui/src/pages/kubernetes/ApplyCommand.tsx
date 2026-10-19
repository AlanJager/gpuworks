import { Typography } from "antd";
import { useTranslation } from "react-i18next";
import CopyButton from "../../components/CopyButton";

/** A command to run in the cluster, with a button that copies it. */
export default function ApplyCommand({ command }: { command: string }) {
  const { t } = useTranslation("kubernetes");
  return (
    <div>
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between" }}>
        <Typography.Text strong>{t("created.command")}</Typography.Text>
        <CopyButton type="default" size="small" label={t("created.copyCommand")} text={command}>{t("created.copyCommand")}</CopyButton>
      </div>
      <pre
        aria-label={t("created.command")}
        tabIndex={0}
        style={{ background: "#0d1117", color: "#e6edf3", padding: 16, borderRadius: 2, overflowX: "auto", whiteSpace: "pre-wrap", wordBreak: "break-all", marginTop: 8 }}
      >
        {command}
      </pre>
    </div>
  );
}

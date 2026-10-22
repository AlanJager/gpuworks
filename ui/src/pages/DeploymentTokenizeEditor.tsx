import { Space, Switch, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type Deployment } from "../api";
import { useOrder } from "./useOrder";

// TokenizeEditor is the deployment detail's tokenize switch: whether the
// gateway forwards /v1/tokenize and /v1/detokenize. A change takes effect on
// the next request; nothing restarts.
export function TokenizeEditor({
  dep,
  onChanged,
}: {
  dep: Deployment;
  onChanged: () => void;
}) {
  const { t } = useTranslation("common");
  const { busy, run } = useOrder();
  const save = async (on: boolean) => {
    const refused = await run(() => api.setDeploymentTokenize(dep.id, on));
    if (refused !== null) {
      message.error(refused);
      return;
    }
    message.success(t("tokenize.saved"));
    onChanged();
  };
  return (
    <Space size={8} wrap>
      <Switch
        size="small"
        checked={dep.tokenize_enabled}
        loading={busy}
        aria-label={t("tokenize.enable")}
        onChange={(on) => void save(on)}
      />
      <Typography.Text type={dep.tokenize_enabled ? undefined : "secondary"}>
        {dep.tokenize_enabled ? t("tokenize.on") : t("tokenize.off")}
      </Typography.Text>
    </Space>
  );
}

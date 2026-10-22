import { useState } from "react";
import { Alert, Button } from "antd";
import { useTranslation } from "react-i18next";
import type { MovedLaunchFlag } from "../../api";

// A dictionary flag written by hand in the argument box belongs in its field:
// the server moves it there when it materializes the launch configuration and
// reports the move (ADR 0020 decision 2). The form's fields and the argument
// text are the server's form_values answer - this notice only says which keys
// moved, so the rewrite is never silent. The line can be dismissed.
export default function DeployMovedFlagsNotice({
  moved,
}: {
  moved?: MovedLaunchFlag[];
  responseSeq?: number;
}) {
  const { t } = useTranslation("deploy");
  const signature = JSON.stringify(moved ?? []);
  const [dismissed, setDismissed] = useState("");
  if (!moved?.length || dismissed === signature) return null;
  return (
    <Alert
      className="deploy-moved-flags"
      type="info"
      showIcon
      message={t("launchConfig.moved", {
        keys: moved.map((item) => item.key).join(", "),
      })}
      action={
        <Button
          size="small"
          type="text"
          onClick={() => setDismissed(signature)}
        >
          {t("launchConfig.movedDismiss")}
        </Button>
      }
    />
  );
}

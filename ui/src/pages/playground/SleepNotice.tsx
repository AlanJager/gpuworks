import type { ReactNode } from "react";
import { Alert } from "antd";
import { useTranslation } from "react-i18next";
import type { PlaygroundModel } from "./types";

// SleepNotice says what a sleeping model means for the next request, as the
// server tells it (wakes_on_request): a sleep the platform chose ends when a
// request comes, so sending is enough; an operator's or a schedule's sleep
// does not, so the model has to be woken first. action is the page's own way
// to wake it, when it has one; without one the notice points to Deployments.
export default function SleepNotice({ model, action, className }: { model?: PlaygroundModel; action?: ReactNode; className?: string }) {
  const { t } = useTranslation("playground");
  if (!model?.sleeping) return null;
  if (model.wakes_on_request) return <Alert className={className} type="info" showIcon message={t("sleep.wakesOnRequest")} />;
  return <Alert className={className} type="warning" showIcon message={t(action ? "sleep.held" : "sleep.heldElsewhere")} action={action} />;
}

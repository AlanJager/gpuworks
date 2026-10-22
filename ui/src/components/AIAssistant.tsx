import { Button, Tooltip } from "antd";
import { RobotOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { useOptionalAssistant, type AssistantSubject, type AssistantTask } from "../assistant/AssistantContext";

// AIAssistant is a one-click entry to the assistant: it starts task about
// subject in the resident side panel. The user writes no prompt; the server
// gathers the subject's context for the task. Outside the app shell (no
// assistant provided) it renders nothing rather than a dead button.
export default function AIAssistant({
  task,
  subject,
  buttonText,
  buttonType = "default",
  buttonSize = "middle",
}: {
  task: AssistantTask;
  subject: AssistantSubject;
  buttonText?: string;
  buttonType?: "default" | "primary" | "link";
  buttonSize?: "small" | "middle" | "large";
}) {
  const { t } = useTranslation("ai");
  const assistant = useOptionalAssistant();
  const label = buttonText || t(`task.${task}.action`);
  if (!assistant) return null;
  return (
    <Tooltip title={t("tooltip")}>
      <Button type={buttonType} size={buttonSize} icon={<RobotOutlined />} onClick={() => assistant.start(task, subject)} aria-label={label}>
        {buttonText ?? label}
      </Button>
    </Tooltip>
  );
}

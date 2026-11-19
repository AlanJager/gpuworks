import { useTranslation } from "react-i18next";
import { useOptionalAssistant, type AssistantSubject, type AssistantTask } from "../assistant/AssistantContext";
import { Button } from "./ui/button";
import { Tooltip } from "./ui/tooltip";
import { ConsoleIcon } from "../ui-kit/consoleIcons";

const zeronSize = { small: "xs", middle: "sm", large: "md" } as const;
const buttonVariant = { default: "tertiary", primary: "primary", link: "link", inline: "link", tertiary: "tertiary" } as const;

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
  buttonType?: "default" | "primary" | "link" | "inline" | "tertiary";
  buttonSize?: "small" | "middle" | "large";
}) {
  const { t } = useTranslation("ai");
  const assistant = useOptionalAssistant();
  const label = buttonText || t(`task.${task}.action`);
  if (!assistant) return null;
  const start = () => assistant.start(task, subject);
  // "inline" and "tertiary" always show the label; the others show
  // buttonText as given and keep the label as the accessible name.
  const labelled = buttonType === "inline" || buttonType === "tertiary";
  return (
    <Tooltip content={t("tooltip")}>
      <Button variant={buttonVariant[buttonType]} size={zeronSize[buttonSize]} leadingIcon={ConsoleIcon.assistant} onClick={start} aria-label={labelled ? undefined : label}>
        {labelled ? label : buttonText ?? label}
      </Button>
    </Tooltip>
  );
}

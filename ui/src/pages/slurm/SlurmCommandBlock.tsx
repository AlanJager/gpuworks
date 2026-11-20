import { useTranslation } from "react-i18next";
import CopyButton from "../../components/CopyButton";

/** A command to run as root on the Slurm controller, under what it is for, with a button that copies it. */
export default function SlurmCommandBlock({ command, title, hint }: { command: string; title: string; hint?: string }) {
  const { t } = useTranslation("slurm");
  return (
    <div className="flex min-w-0 flex-col gap-1.5">
      <div className="flex items-center justify-between gap-2">
        <strong className="font-semibold text-fg-default">{title}</strong>
        <CopyButton type="default" size="small" label={t("command.copy", { title })} text={command}>{t("command.copyShort")}</CopyButton>
      </div>
      {hint && <span className="text-fg-muted">{hint}</span>}
      <pre aria-label={title} tabIndex={0} className="code-block m-0 whitespace-pre-wrap break-all p-3">{command}</pre>
    </div>
  );
}

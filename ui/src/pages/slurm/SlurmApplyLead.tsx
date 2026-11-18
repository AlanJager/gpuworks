import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { SlurmCluster } from "../../slurmApi";
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from "../../components/ui/accordion";
import { Button } from "../../components/ui/button";
import SlurmApplyModal from "./SlurmApplyModal";
import SlurmSetupCommand from "./SlurmSetupCommand";

/**
 * What an entry the platform can set up itself leads with until a node has
 * joined: the labelled "Check and apply" button (the check that changes
 * nothing, then the operator's confirmation) with one line saying where it
 * runs. The commands to run by hand stay available, folded away under their
 * own name. Whether the platform can apply is the server's word on the entry
 * (`can_apply`); the caller shows this only then.
 */
export default function SlurmApplyLead({ cluster, onChanged }: { cluster: SlurmCluster; onChanged?: () => void }) {
  const { t } = useTranslation("slurm");
  const [applying, setApplying] = useState(false);
  const [manual, setManual] = useState(false);
  return (
    <div className="flex max-w-[880px] flex-col gap-2">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <Button variant="primary" size="sm" onClick={() => setApplying(true)}>{t("actions.apply")}</Button>
        <span className="text-fg-muted">{t("detail.applyHelp")}</span>
      </div>
      {cluster.setup_command && (
        <Accordion type="single" collapsible value={manual ? "manual" : ""} onValueChange={(next: string) => setManual(next === "manual")}>
          <AccordionItem value="manual">
            <AccordionTrigger>{t("detail.manual")}</AccordionTrigger>
            <AccordionContent>{manual && <SlurmSetupCommand command={cluster.setup_command} checkCommand={cluster.check_command} />}</AccordionContent>
          </AccordionItem>
        </Accordion>
      )}
      {applying && <SlurmApplyModal cluster={cluster} onClose={() => setApplying(false)} onDone={onChanged} />}
    </div>
  );
}

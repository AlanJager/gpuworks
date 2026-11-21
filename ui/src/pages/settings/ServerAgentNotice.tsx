import { useTranslation } from "react-i18next";
import type { AgentFleetSummary } from "../../api";
import { Notice } from "../../ui-kit/Notice";
import { Button } from "../../components/ui/button";

/**
 * After a server upgrade: how many nodes still run another Agent version, and
 * the way to the Nodes page, already filtered to them, where they are upgraded.
 */
export default function ServerAgentNotice({ summary, dismiss }: { summary: AgentFleetSummary; dismiss: () => void }) {
  const { t } = useTranslation("settings");
  const { t: nodes } = useTranslation("nodes");
  const { t: common } = useTranslation("common");
  const { target, align, check } = summary;
  const group = align ? "align" : check ? "check" : "";
  const title = align ? t("updates.agentFollowup.upgrade", { version: target, count: align })
    : check ? t("updates.agentFollowup.blocked", { version: target, count: check })
    : t("updates.agentFollowup.none", { version: target });
  const facts = [
    !!align && nodes("fleet.fact.align", { count: align }),
    !!check && nodes("fleet.fact.check", { count: check }),
  ].filter(Boolean).join(" · ");
  return <Notice tone="default" title={title} action={<>
    <Button asChild variant="link" size="sm"><a href={group ? `/nodes?agent_upgrade=${group}` : "/nodes"} onClick={dismiss}>{t("updates.viewNodes")}</a></Button>
    <Button variant="ghost" size="sm" onClick={dismiss}>{common("actions.close")}</Button>
  </>}>
    {/* Both groups are worth a line only when the title names one of them. */}
    {!!align && !!check && <span data-part="facts">{facts}</span>}
  </Notice>;
}

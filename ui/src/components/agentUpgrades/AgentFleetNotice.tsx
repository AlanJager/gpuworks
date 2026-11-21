import { useState } from "react";
import { Link } from "react-router-dom";
import { useSessionUser } from "../sessionUser";
import { useTranslation } from "react-i18next";
import type { AgentFleetSummary } from "../../api";
import { Notice } from "../../ui-kit/Notice";
import { Button } from "../ui/button";

/**
 * Says that nodes run an Agent other than the server's target, and offers the
 * way to fix it: upgrade every node that can be, or look at the ones that
 * cannot. onUpgradeAll is absent for a user who may not upgrade.
 */
export default function AgentFleetNotice({ summary, failed, retry, fleetSearch, onUpgradeAll }: {
  summary?: AgentFleetSummary;
  failed: boolean;
  retry: () => void;
  fleetSearch: (group: string) => string;
  onUpgradeAll?: () => void;
}) {
  const { t } = useTranslation("nodes");
  const userID = useSessionUser()?.id;
  if (failed) return <Notice tone="warning" title={t("fleet.loadError")}
    action={<Button size="sm" variant="ghost" onClick={retry}>{t("fleet.retry")}</Button>} />;
  if (!summary) return null;
  const { target, align, check, run_id } = summary;
  if (!target || target === "dev") return <Notice title={t("fleet.noTarget")} />;
  if (!align && !check && !run_id) return null;
  const key = `gpuworks.agent-notice.${userID}.${target}`;
  return <TargetNotice key={key} storageKey={userID === undefined ? undefined : key} summary={summary} fleetSearch={fleetSearch} onUpgradeAll={onUpgradeAll} />;
}

function TargetNotice({ storageKey, summary, fleetSearch, onUpgradeAll }: {
  storageKey?: string;
  summary: AgentFleetSummary;
  fleetSearch: (group: string) => string;
  onUpgradeAll?: () => void;
}) {
  const { t } = useTranslation("nodes");
  const { target, align, check, run_id } = summary;
  const [ignored, setIgnored] = useState(() => {
    try { return !!storageKey && window.localStorage.getItem(storageKey) === "1"; }
    catch { return false; }
  });
  if (ignored) return null;
  const ignore = () => {
    setIgnored(true);
    try { if (storageKey) window.localStorage.setItem(storageKey, "1"); }
    catch { /* The notice is still dismissed for this visit. */ }
  };
  const title = align ? t("fleet.mismatch", { count: align }) : check ? t("fleet.blockedOnly", { count: check }) : t("fleet.upgrading");
  const facts = [
    t("fleet.fact.target", { target }),
    !!align && t("fleet.fact.align", { count: align }),
    !!check && t("fleet.fact.check", { count: check }),
  ].filter(Boolean).join(" · ");
  return <Notice tone="default" title={title}
    action={<>
      {/* One batch at a time: while one runs the way forward is its progress. */}
      {!!align && !run_id && onUpgradeAll && <Button size="sm" onClick={onUpgradeAll}>{t("fleet.upgradeAll")}</Button>}
      <Button variant="ghost" size="sm" onClick={ignore}>{t("fleet.ignore")}</Button>
    </>}>
    <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
      <span data-part="facts">{facts}</span>
      {!!align && <Button asChild variant="link" size="sm"><Link to={{ search: fleetSearch("align") }}>{t("fleet.viewNodes")}</Link></Button>}
      {!!check && <Button asChild variant="link" size="sm"><Link to={{ search: fleetSearch("check") }}>{t("fleet.viewBlocked")}</Link></Button>}
      {!!run_id && <Button asChild variant="link" size="sm">
        <a href={`/nodes?agent_run=${run_id}`}>{t("fleet.progress")}</a>
      </Button>}
    </div>
  </Notice>;
}

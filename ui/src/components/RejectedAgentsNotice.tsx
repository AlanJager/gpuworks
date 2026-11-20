import { useTranslation } from "react-i18next";
import { api, usePolling, type AgentRejectEvent } from "../api";
import { formatTime } from "../i18n/datetime";
import { Notice } from "../ui-kit/Notice";
import { HelpTip } from "../ui-kit/HelpTip";
import { Button } from "./ui/button";

// An Agent refused for its credential retries every five minutes, so a source
// that has been quiet this long has been dealt with (or its Agent stopped).
const RECENT_MS = 30 * 60 * 1000;
const SHOWN = 5;

export interface RejectedAgent {
  host: string;
  tokenPrefix: string;
  count: number;
  first: string;
  last: string;
}

/** The address without its port: the port of an outbound connection changes with every attempt. */
function hostOf(remote: string): string {
  const value = remote.trim();
  const bracketed = /^\[([^\]]+)\](?::\d+)?$/.exec(value);
  if (bracketed) return bracketed[1];
  const single = /^([^:]+):\d+$/.exec(value);
  return single ? single[1] : value;
}

/**
 * Groups the server's recent handshake refusals for an unknown credential by
 * the host they came from and the credential prefix they presented, newest
 * first. Sources whose last attempt is older than RECENT_MS are left out.
 */
export function rejectedAgents(events: AgentRejectEvent[], now: number): RejectedAgent[] {
  const groups = new Map<string, RejectedAgent>();
  for (const event of events) {
    if (event.reason !== "bad_token") continue;
    const at = Date.parse(event.at);
    if (Number.isNaN(at)) continue;
    const host = hostOf(event.remote);
    const key = `${host}\n${event.token_prefix}`;
    const group = groups.get(key);
    if (!group) {
      groups.set(key, { host, tokenPrefix: event.token_prefix, count: 1, first: event.at, last: event.at });
      continue;
    }
    group.count += 1;
    if (at < Date.parse(group.first)) group.first = event.at;
    if (at > Date.parse(group.last)) group.last = event.at;
  }
  return [...groups.values()]
    .filter((group) => now - Date.parse(group.last) <= RECENT_MS)
    .sort((a, b) => Date.parse(b.last) - Date.parse(a.last));
}

/**
 * Says which hosts run an Agent this server refuses for its credential, and
 * what to do about it. Nothing is shown while there is none, or while the
 * list cannot be read.
 */
export default function RejectedAgentsNotice({ onAddNode }: { onAddNode?: () => void }) {
  const { t } = useTranslation("nodes");
  // A list that cannot be read shows nothing: this notice is advice beside
  // the node list, which reports an unreachable server itself, and the next
  // poll asks again.
  const { data } = usePolling(api.agentRejects, 30000);
  const agents = rejectedAgents(data?.items ?? [], Date.now());
  if (agents.length === 0) return null;
  return <Notice tone="warning"
    title={<>{t("rejects.refused", { count: agents.length })} <HelpTip content={t("rejects.refusedHelp")} /></>}
    action={onAddNode && <Button size="sm" variant="ghost" onClick={onAddNode}>{t("action.add")}</Button>}>
    <ul className="m-0 list-none p-0">
      {agents.slice(0, SHOWN).map((agent) => <li key={`${agent.host}\n${agent.tokenPrefix}`}>
        {t("rejects.source", {
          host: agent.host || t("rejects.unknownAddress"),
          prefix: agent.tokenPrefix || t("rejects.emptyToken"),
          count: agent.count,
          first: formatTime(agent.first),
          last: formatTime(agent.last),
        })}
      </li>)}
      {agents.length > SHOWN && <li>{t("rejects.more", { count: agents.length - SHOWN })}</li>}
    </ul>
  </Notice>;
}

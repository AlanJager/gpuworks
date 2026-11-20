import { useTranslation } from "react-i18next";
import type { SlurmReach } from "../../slurmApi";
import { slurmReachFailure, slurmReachUnasked } from "./slurmReach";

/**
 * Whether the server's address can be reached, host by host, from a check or
 * an apply: each host that cannot, with why; the hosts that could not be
 * asked; and why the compute nodes were not listed to ask them. When every
 * host asked reached it, one line says so. Nothing here decides anything.
 */
export default function SlurmReachView({ reach, url }: { reach: SlurmReach; url: string }) {
  const { t } = useTranslation("slurm");
  const failed = reach.hosts.filter((host) => host.status === "failed");
  const unasked = reach.hosts.filter((host) => host.status === "unknown");
  const reached = reach.hosts.filter((host) => host.status === "ok").length;
  if (reach.hosts.length === 0 && !reach.nodes_unlisted) return null;
  return (
    <section className="flex flex-col gap-1.5">
      <strong className="font-semibold text-fg-default">{t("reach.title")}</strong>
      {failed.length > 0 ? (
        <ul aria-label={t("reach.failedList")} className="m-0 list-disc ps-5">
          {failed.map((host) => <li key={`${host.role}/${host.name}`}>{slurmReachFailure(t, url, host)}</li>)}
        </ul>
      ) : reached > 0 && <span>{t("reach.allReach", { count: reached, url })}</span>}
      {unasked.length > 0 && (
        <div className="flex flex-col gap-0.5">
          <span className="text-fg-muted">{t("reach.unasked")}</span>
          <span className="break-words">{unasked.map((host) => slurmReachUnasked(t, host)).join(", ")}</span>
        </div>
      )}
      {reach.nodes_unlisted && <span className="text-fg-muted">{t(`reach.unlisted.${reach.nodes_unlisted}`, { permission: reach.nodes_permission ?? "" })}</span>}
    </section>
  );
}

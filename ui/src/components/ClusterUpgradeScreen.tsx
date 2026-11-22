import { useEffect, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import {
  isClusterUpgradeActive, readClusterUpgradeProgress, subscribeClusterUpgrade,
  type ClusterUpgradeProgress,
} from "../clusterUpgrade";

const POLL_MS = 2000;
const KNOWN_STATES = new Set(["waiting", "window", "migrating", "idle", "unobserved"]);

/**
 * ClusterUpgradeBoundary replaces the console with the upgrade screen from
 * the first API answer that carries the upgrade code until a member serves
 * again, then reloads: the new release serves other console assets.
 */
export default function ClusterUpgradeBoundary({ children, reload = () => window.location.reload() }: { children: ReactNode; reload?: () => void }) {
  const [active, setActive] = useState(isClusterUpgradeActive);
  useEffect(() => subscribeClusterUpgrade(() => setActive(true)), []);
  if (!active) return <>{children}</>;
  return <ClusterUpgradeScreen reload={reload} />;
}

function ClusterUpgradeScreen({ reload }: { reload: () => void }) {
  const { t } = useTranslation("clusterUpgrade");
  const [progress, setProgress] = useState<ClusterUpgradeProgress>();
  const [readable, setReadable] = useState(true);

  useEffect(() => {
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    let stopped = false;
    const poll = async () => {
      const result = await readClusterUpgradeProgress(controller.signal);
      if (stopped) return;
      if (result.outcome === "done") {
        reload();
        return;
      }
      setReadable(result.outcome !== "unknown");
      if (result.progress) setProgress(result.progress);
      timer = setTimeout(() => void poll(), POLL_MS);
    };
    void poll();
    return () => {
      stopped = true;
      controller.abort();
      if (timer) clearTimeout(timer);
    };
  }, [reload]);

  const state = !readable || !progress ? "unobserved" : progress.state;
  const stateText = KNOWN_STATES.has(state) ? t(`states.${state}`) : state;
  return (
    <main className="auth-page" role="status" aria-live="polite">
      <div className="w-full max-w-[520px] rounded-lg bg-surface p-8 shadow-sm">
        <h1 className="m-0 text-xl font-semibold">{t("title")}</h1>
        <p className="mb-0 mt-2 text-secondary">{t("body")}</p>
        <dl className="mb-0 mt-5 grid grid-cols-[auto_1fr] gap-x-4 gap-y-2">
          <dt className="text-secondary">{t("state")}</dt>
          <dd className="m-0">{stateText}</dd>
          <dt className="text-secondary">{t("schema")}</dt>
          <dd className="m-0 tabular-nums">{progress ? t("count", { done: progress.schema.applied, total: progress.schema.known }) : "—"}</dd>
          <dt className="text-secondary">{t("members")}</dt>
          <dd className="m-0 tabular-nums">{progress ? t("count", { done: progress.members.on_target, total: progress.members.total }) : "—"}</dd>
        </dl>
      </div>
    </main>
  );
}

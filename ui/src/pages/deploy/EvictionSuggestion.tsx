import { Alert, Button, Typography } from "antd";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type EvictionSuggestion as Suggestion } from "../../api";
import { latestSleepFailure } from "../instanceEvents";
import { isFollower } from "../instanceGroups";
import { apiErrorText } from "../../apiError";
import SleepModeDialog, { type SleepMode } from "../../components/SleepModeDialog";

/** How long the page waits for the agent to report the sleep before giving up. */
export const evictionSleepTimeoutMs = 10 * 60 * 1000;
const pollIntervalMs = 2000;

type Phase = { kind: "idle" } | { kind: "sleeping" } | { kind: "slept" } | { kind: "failed"; detail: string } | { kind: "timeout" };

/**
 * One "sleep X to make room" suggestion from the evaluation, with the button
 * that carries it out: order an L1 sleep, wait until every running instance
 * of that deployment that leads a replica reports asleep (a multi-node
 * replica's followers sleep with their leader and report no level of their
 * own), then ask the form to evaluate again against the ledger that now has
 * the card free.
 */
export default function EvictionSuggestionAlert({ suggestion, onSlept, deploymentFetcher = api.deployment, sleeper = api.sleepDeployment, deploymentEventFetcher = api.deploymentEvents, pollMs = pollIntervalMs, timeoutMs = evictionSleepTimeoutMs }: {
  suggestion: Suggestion;
  onSlept?: () => void;
  deploymentFetcher?: typeof api.deployment;
  sleeper?: typeof api.sleepDeployment;
  deploymentEventFetcher?: typeof api.deploymentEvents;
  pollMs?: number;
  timeoutMs?: number;
}) {
  const { t } = useTranslation("deploy");
  const { t: tCommon } = useTranslation("common");
  const [phase, setPhase] = useState<Phase>({ kind: "idle" });
  const [choosingMode, setChoosingMode] = useState(false);
  const alive = useRef(true);
  useEffect(() => () => { alive.current = false; }, []);

  const start = async (mode: SleepMode) => {
    setChoosingMode(false);
    setPhase({ kind: "sleeping" });
    // Capture the server-side event id before ordering. Event ids are the
    // watermark; client clocks are not reliable across nodes.
    let baseline = 0;
    try {
      const before = await deploymentEventFetcher(suggestion.deployment_id, { limit: 1 });
      baseline = before.items[0]?.id ?? 0;
    } catch {
      // The order can still be sent when the optional history read is down.
    }
    try {
      await sleeper(suggestion.deployment_id, 1, mode);
    } catch (e) {
      if (alive.current) setPhase({ kind: "failed", detail: apiErrorText(tCommon, e) });
      return;
    }
    const deadline = Date.now() + timeoutMs;
    while (alive.current && Date.now() < deadline) {
      await new Promise((resolve) => setTimeout(resolve, pollMs));
      let asleep = false;
      try {
        const dep = await deploymentFetcher(suggestion.deployment_id);
        const live = (dep.instances ?? []).filter((i) => i.state === "running" && !isFollower(i));
        asleep = live.length > 0 && live.every((i) => (i.sleep_level ?? 0) > 0);
        if (!asleep) {
          const events = await deploymentEventFetcher(suggestion.deployment_id, { limit: 50 }).then((page) => page.items, () => []);
          const failure = latestSleepFailure(events, baseline);
          if (failure) {
            if (alive.current) setPhase({ kind: "failed", detail: failure });
            return;
          }
        }
      } catch {
        // A transient fetch failure is not a verdict; keep polling until the deadline.
      }
      if (asleep) {
        if (alive.current) {
          setPhase({ kind: "slept" });
          onSlept?.();
        }
        return;
      }
    }
    if (alive.current) setPhase({ kind: "timeout" });
  };

  const description = (() => {
    switch (phase.kind) {
      case "sleeping":
        return <Typography.Text>{t("eval.evictionSleeping", { deployment: suggestion.deployment })}</Typography.Text>;
      case "slept":
        return <Typography.Text type="success">{t("eval.evictionSlept", { deployment: suggestion.deployment })}</Typography.Text>;
      case "failed":
        return <Typography.Text type="danger">{t("eval.evictionFailed", { deployment: suggestion.deployment, detail: phase.detail })}</Typography.Text>;
      case "timeout":
        return <Typography.Text type="danger">{t("eval.evictionTimeout", { deployment: suggestion.deployment, minutes: Math.round(timeoutMs / 60000) })}</Typography.Text>;
      default:
        return t("eval.evictionDescription", { gpus: suggestion.gpu_indexes.join(", ") });
    }
  })();

  return (
    <>
      <Alert
        type="info"
        showIcon
        message={t("eval.evictionMessage", { deployment: suggestion.deployment, gib: suggestion.frees_gib.toFixed(1) })}
        description={description}
        action={
          phase.kind === "slept" ? null : (
            <Button size="small" type="primary" loading={phase.kind === "sleeping"} onClick={() => setChoosingMode(true)}>
              {t("eval.sleepAndReevaluate")}
            </Button>
          )
        }
      />
      <SleepModeDialog open={choosingMode} level={1} onConfirm={start} onCancel={() => setChoosingMode(false)} />
    </>
  );
}

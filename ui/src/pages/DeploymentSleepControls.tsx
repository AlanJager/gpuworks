import { useEffect, useState } from "react";
import { Alert, Button, Space, Tag, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type Deployment, type WakeResult } from "../api";
import { APIError } from "../apiError";
import SleepModeDialog, { type SleepMode } from "../components/SleepModeDialog";
import { latestSleepFailure } from "./instanceEvents";
import { sleepSummary } from "./instanceGroups";
import { useOrder } from "./useOrder";

// SleepControls are the drawer's sleep and wake orders for the whole
// deployment. The summary counts replicas: all asleep, some asleep (after an
// order for one replica or one PD role), or none. A wake the cards cannot
// take back all at once can wake what fits instead. A PD deployment also
// sleeps and wakes the roles its mode runs without.
export default function SleepControls({ dep, onDone }: { dep: Deployment; onDone: () => void }) {
  const { t } = useTranslation("deployments");
  const { busy, run } = useOrder();
  const [pendingLevel, setPendingLevel] = useState<1 | 2 | null>(null);
  const [sleepFailure, setSleepFailure] = useState<string>();
  const [failureAfterID, setFailureAfterID] = useState(0);
  const [eventsRefresh, setEventsRefresh] = useState(0);
  const [wakeRefused, setWakeRefused] = useState(false);
  const instances = dep.instances ?? [];
  // The list refreshes asynchronously after an order. Re-read the event
  // history whenever an instance reports a new state, phase, sleep level or
  // sleep order so a failure that arrives after the order is not missed by a
  // one-shot fetch.
  const sleepStatusKey = instances
    .map((instance) => `${instance.uuid}:${instance.state}:${instance.phase ?? ""}:${instance.sleep_level ?? 0}:${instance.desired_sleep ?? 0}`)
    .join("|");
  const summary = sleepSummary(instances);
  useEffect(() => {
    let active = true;
    api.deploymentEvents(dep.id, { limit: 50 }).then((page) => page.items, () => [])
      .then((events) => {
        if (!active) return;
        setSleepFailure(latestSleepFailure(events, failureAfterID));
      });
    return () => { active = false; };
  }, [dep.id, eventsRefresh, failureAfterID, sleepStatusKey]);
  const show = dep.sleepable || summary.asleep > 0;
  if (!show) return null;

  const levelName = (lv: number) => (lv === 1 ? t("sleep.quick") : lv === 2 ? t("sleep.deep") : `L${lv}`);

  // ordered sends an order and reports whether the server took it. A taken
  // order hides the failures recorded before it; a refused one leaves the
  // last failure shown.
  const ordered = async (send: () => Promise<unknown>, failKey: string): Promise<boolean> => {
    let baseline = 0;
    const refused = await run(async () => {
      const before = await api.deploymentEvents(dep.id, { limit: 1 }).then((page) => page.items, () => []);
      baseline = before[0]?.id ?? 0;
      await send();
    });
    if (refused !== null) {
      message.error(t(failKey, { detail: refused }));
      return false;
    }
    setFailureAfterID(baseline);
    setSleepFailure(undefined);
    setEventsRefresh((value) => value + 1);
    onDone();
    return true;
  };

  const doSleep = async (level: 1 | 2, mode: SleepMode) => {
    if (!(await ordered(() => api.sleepDeployment(dep.id, level, mode), "sleep.failed"))) return;
    message.success(level === 1 ? t("sleep.quickStarted") : t("sleep.deepStarted"));
    setPendingLevel(null);
    setWakeRefused(false);
  };

  const doWake = async (partial: boolean) => {
    let result: WakeResult | undefined;
    let code: string | undefined;
    const woke = await ordered(async () => {
      try {
        result = await api.wakeDeployment(dep.id, { partial });
      } catch (e) {
        code = e instanceof APIError ? e.code : undefined;
        throw e;
      }
    }, "sleep.wakeFailed");
    if (!woke) {
      // Cards that cannot take every sleeper back may still take some.
      setWakeRefused(!partial && summary.asleep > 1 && code === "WAKE_INSUFFICIENT_GPU_MEMORY");
      return;
    }
    // Counted in replicas, as the summary is: the server lists every rank of
    // a woken multi-node replica, and refuses each replica whole (one entry).
    const refused = result?.refused?.length ?? 0;
    const woken = replicaCount(instances, result?.woken ?? []);
    message.success(partial ? t("sleep.partialWoken", { woken, refused }) : t("sleep.wakeStarted"));
    setWakeRefused(partial && refused > 0);
  };

  const all = summary.total > 0 && summary.asleep === summary.total;
  return (
    <Space style={{ marginTop: 16 }} wrap>
      {sleepFailure && (
        <Alert
          type="error"
          showIcon
          message={t("sleep.lastFailure")}
          description={sleepFailure}
          style={{ flexBasis: "100%" }}
        />
      )}
      {all && <Tag color="orange">{t("sleep.sleeping", { level: summary.level, name: levelName(summary.level) })}</Tag>}
      {summary.asleep > 0 && !all && <Tag color="orange">{t("sleep.partial", { asleep: summary.asleep, total: summary.total })}</Tag>}
      {summary.asleep > 0 && (
        <Button size="small" type="primary" loading={busy} onClick={() => void doWake(false)}>
          {all ? t("sleep.wake") : t("sleep.wakeAll")}
        </Button>
      )}
      {wakeRefused && summary.asleep > 0 && (
        <Button size="small" loading={busy} onClick={() => void doWake(true)}>
          {t("sleep.wakeWhatFits")}
        </Button>
      )}
      {!all && (
        <>
          <Button size="small" loading={busy} onClick={() => setPendingLevel(1)}>
            {t("sleep.quickAction")}
          </Button>
          <Button size="small" loading={busy} onClick={() => setPendingLevel(2)}>
            {t("sleep.deepAction")}
          </Button>
        </>
      )}
      {dep.pd && dep.sleepable && <RoleSleepActions dep={dep} onDone={onDone} />}
      <SleepModeDialog
        open={pendingLevel !== null}
        level={pendingLevel ?? 1}
        confirming={busy}
        onConfirm={(mode) => pendingLevel && void doSleep(pendingLevel, mode)}
        onCancel={() => setPendingLevel(null)}
      />
    </Space>
  );
}

// RoleSleepActions sleep (memory / L1) or wake one role of a PD deployment:
// the roles the server says its mode runs without (may_be_empty). With every
// prefill instance asleep the decoders answer alone.
function RoleSleepActions({ dep, onDone }: { dep: Deployment; onDone: () => void }) {
  const { t } = useTranslation("deployments");
  const { run } = useOrder();
  const [busy, setBusy] = useState<string | null>(null);
  const [asking, setAsking] = useState<string | null>(null);
  const instances = dep.instances ?? [];
  const order = async (role: string, send: () => Promise<unknown>, done: string, failKey: string) => {
    setBusy(role);
    const refused = await run(send);
    setBusy(null);
    if (refused !== null) {
      message.error(t(failKey, { detail: refused }));
      return;
    }
    message.success(done);
    setAsking(null);
    onDone();
  };
  return (
    <>
      {(dep.pd?.roles ?? []).filter((role) => role.may_be_empty).map(({ role }) => {
        const summary = sleepSummary(instances, role);
        if (summary.total === 0) return null;
        const roleName = t(`detail.pd.role.${role}`, { defaultValue: role });
        return (
          <Space key={role} size={4}>
            {summary.asleep > 0 && <Tag color="orange">{t("sleep.role.summary", { role: roleName, asleep: summary.asleep, total: summary.total })}</Tag>}
            {summary.asleep > 0 ? (
              <Button size="small" loading={busy === role} onClick={() => void order(role, () => api.wakeDeployment(dep.id, { role }), t("sleep.wakeStarted"), "sleep.wakeFailed")}>
                {t("sleep.role.wake", { role: roleName })}
              </Button>
            ) : (
              <Button size="small" loading={busy === role} onClick={() => setAsking(role)}>
                {t("sleep.role.sleep", { role: roleName })}
              </Button>
            )}
          </Space>
        );
      })}
      <SleepModeDialog
        open={asking !== null}
        level={1}
        confirming={busy !== null}
        onConfirm={(sleepMode) => asking && void order(asking, () => api.sleepDeployment(dep.id, 1, sleepMode, asking), t("sleep.quickStarted"), "sleep.failed")}
        onCancel={() => setAsking(null)}
      />
    </>
  );
}

// replicaCount is how many replicas the named instances fill: a multi-node
// replica's ranks count once, by the replica the server says each fills.
function replicaCount(instances: Deployment["instances"], uuids: string[]): number {
  const named = new Set(uuids);
  const replicas = new Set<string>();
  for (const inst of instances ?? []) {
    if (!named.has(inst.uuid)) continue;
    replicas.add(inst.replica ? `${inst.replica.role ?? ""}#${inst.replica.index}` : inst.uuid);
  }
  return replicas.size;
}

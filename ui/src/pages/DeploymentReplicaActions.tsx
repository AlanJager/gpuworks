import { useState } from "react";
import { Alert, Button, Checkbox, InputNumber, Modal, Popconfirm, Space, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type Deployment, type Instance, type Node, type PdScaleUnitInput, type ScheduleMessage } from "../api";
import NodePicker from "../nodes/NodePicker";
import { apiErrorText } from "../apiError";
import AutoReplicasDialog, { placeOnRefusal } from "./DeploymentAutoReplicas";
import { scheduleText } from "./deploy/deploySchedule";
import SleepModeDialog, { type SleepMode } from "../components/SleepModeDialog";
import { ConfirmOrderModal } from "./OrderDialogs";
import { useOrder } from "./useOrder";
import { isFollower } from "./instanceGroups";

// replicaCount is how many replicas the deployment asks for: a PD
// deployment's are its roles' units.
function replicaCount(dep: Deployment): number {
  if (dep.pd) return dep.pd.roles.reduce((sum, role) => sum + role.desired, 0);
  return dep.replicas;
}

// ReplicaActions are an instance panel's restart, remove, and sleep or wake
// actions. They show only when the deployment has more than one replica (a
// single one is handled from the drawer header) and only on an instance the
// server says fills a replica; a follower says it goes with its leader. A
// replica is changed through the instance that leads it.
export function ReplicaActions({ dep, inst, onChanged }: { dep: Deployment; inst: Instance; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const [restartOpen, setRestartOpen] = useState(false);
  const [drain, setDrain] = useState(true);
  const removal = useOrder();
  const replica = inst.replica;
  if (dep.desired_state !== "active" || replicaCount(dep) <= 1 || !replica) return null;
  if (isFollower(inst)) {
    return <div style={{ marginTop: 8 }}><Typography.Text type="secondary" style={{ fontSize: 12 }}>{t("replica.followerHint")}</Typography.Text></div>;
  }
  // Removing one named replica is a scale-down of an ordinary deployment: a
  // count naming the instance, or the multi-node list without the replica
  // this instance leads. A PD deployment removes units in Scale roles.
  const multiNode = (dep.nnodes ?? 1) > 1;
  const units = unitInputs(dep);
  const removable = !dep.pd && (!multiNode || replica.index < units.length);
  const remove = async () => {
    const refused = await removal.run(() =>
      api.scaleReplicas(dep.id, multiNode ? { units: units.filter((_, index) => index !== replica.index) } : { replicas: dep.replicas - 1, remove: [inst.uuid] }),
    );
    if (refused !== null) {
      message.error(t("replica.removeFailed", { detail: refused }));
      return;
    }
    message.success(t("replica.removed"));
    onChanged();
  };
  return (
    <Space size={8} style={{ marginTop: 8 }}>
      <Button size="small" onClick={() => setRestartOpen(true)}>{t("replica.restart")}</Button>
      {removable && (
        <Popconfirm title={t("replica.removeConfirm")} onConfirm={() => void remove()} okText={t("replica.remove")} cancelText={t("detail.cancel")}>
          <Button size="small" danger loading={removal.busy}>{t("replica.remove")}</Button>
        </Popconfirm>
      )}
      <ReplicaSleepAction inst={inst} onChanged={onChanged} />
      {/* Draining is the default: the instance finishes the requests it
          holds while its replacement starts. */}
      <ConfirmOrderModal
        open={restartOpen}
        title={t("replica.restartTitle")}
        okText={t("replica.restart")}
        failKey="replica.restartFailed"
        order={() => api.restartInstance(inst.uuid, drain)}
        onClose={() => setRestartOpen(false)}
        onDone={() => {
          message.success(t("replica.restartStarted"));
          onChanged();
        }}
      >
        <Typography.Paragraph>{t("replica.restartDescription")}</Typography.Paragraph>
        <Checkbox checked={drain} onChange={(e) => setDrain(e.target.checked)}>{t("replica.drain")}</Checkbox>
      </ConfirmOrderModal>
    </Space>
  );
}

// ReplicaSleepAction sleeps (memory / L1) or wakes one replica, as the server
// says the replica may on its own (can_sleep_alone, can_wake_alone): not the
// last awake one of a PD role its mode cannot run without. A multi-node
// replica sleeps whole; its followers go with the leader.
function ReplicaSleepAction({ inst, onChanged }: { inst: Instance; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const [asking, setAsking] = useState(false);
  const { busy, run } = useOrder();
  const order = async (send: () => Promise<unknown>, done: string, failKey: string) => {
    const refused = await run(send);
    if (refused !== null) {
      message.error(t(failKey, { detail: refused }));
      return;
    }
    message.success(done);
    setAsking(false);
    onChanged();
  };
  if (inst.asleep) {
    if (!inst.can_wake_alone) return null;
    return (
      <Button size="small" type="primary" loading={busy} onClick={() => void order(() => api.wakeInstance(inst.uuid), t("replica.wakeStarted"), "sleep.wakeFailed")}>
        {t("replica.wake")}
      </Button>
    );
  }
  if (!inst.can_sleep_alone) return null;
  return (
    <>
      <Button size="small" loading={busy} onClick={() => setAsking(true)}>{t("replica.sleep")}</Button>
      <SleepModeDialog
        open={asking}
        level={1}
        confirming={busy}
        onConfirm={(mode: SleepMode) => void order(() => api.sleepInstance(inst.uuid, 1, mode), t("replica.sleepStarted"), "sleep.failed")}
        onCancel={() => setAsking(false)}
      />
    </>
  );
}

// unitInputs is a multi-node deployment's replica list as the scale request
// sends it back.
function unitInputs(dep: Deployment): PdScaleUnitInput[] {
  return (dep.units ?? []).map((unit) => ({ node_uuid: unit.node_uuid, followers: (unit.followers ?? []).map((rank) => ({ node_uuid: rank.node_uuid })) }));
}

// ReplicasEditor edits an ordinary deployment's replicas: a single-node
// deployment's count in place, a multi-node deployment's list in a dialog
// (PD scales its roles). A single-node scale-down leaves the choice of which
// replicas go to the server: not yet ready first, then the newest.
export function ReplicasEditor({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const { t: tDeploy } = useTranslation("deploy");
  const { t: tCommon } = useTranslation("common");
  const [value, setValue] = useState<number | null>(dep.replicas);
  const [firstNodes, setFirstNodes] = useState<string[]>([]);
  const { busy, run } = useOrder();
  const [listOpen, setListOpen] = useState(false);
  const ranks = dep.nnodes ?? 1;
  if (dep.pd || dep.desired_state === "deleting") return <>{dep.replicas}</>;
  if (ranks > 1) {
    return (
      <Space size={8}>
        {dep.replicas}
        <Button size="small" onClick={() => setListOpen(true)}>{t("replica.editUnits")}</Button>
        {listOpen && (dep.placement_mode === "auto"
          ? <AutoReplicasDialog dep={dep} ranks={ranks} onClose={() => setListOpen(false)} onChanged={onChanged} />
          : <MultiNodeReplicasDialog dep={dep} ranks={ranks} onClose={() => setListOpen(false)} onChanged={onChanged} />)}
      </Space>
    );
  }
  const added = value != null && value > dep.replicas ? value - dep.replicas : 0;
  const apply = async () => {
    if (value == null || value === dep.replicas) return;
    // Only as many first-placement nodes as replicas are added count.
    const placeOn = added > 0 ? firstNodes.slice(0, added) : [];
    const refused = await run(async () => {
      try {
        await api.scaleReplicas(dep.id, placeOn.length > 0 ? { replicas: value, place_on: placeOn } : { replicas: value });
      } catch (e) {
        throw placeOnRefusal(e, t, tDeploy, tCommon) ?? e;
      }
    });
    if (refused !== null) {
      message.error(t("replica.scaleFailed", { detail: refused }));
      return;
    }
    message.success(t("replica.scaled", { count: value }));
    onChanged();
  };
  // A node that is offline or cordoned is listed but cannot take a replica.
  const unpickable = (node: Node) => node.state !== "online" || (node.scheduling ?? "schedulable") !== "schedulable";
  return (
    <Space size={8} wrap>
      <InputNumber id="deployment-replicas" size="small" min={1} max={dep.max_replicas} value={value} onChange={(next) => setValue(next)} aria-label={t("detail.replicas")} />
      {added > 0 && (
        <NodePicker
          mode="multiple"
          style={{ minWidth: 220 }}
          size="small"
          maxCount={added}
          placeholder={t("replica.placeOn.placeholder", { count: added })}
          aria-label={t("replica.placeOn.label")}
          value={firstNodes.slice(0, added)}
          optionDisabled={unpickable}
          onChange={(next) => setFirstNodes(Array.isArray(next) ? next : [])}
        />
      )}
      <Button size="small" loading={busy} disabled={value == null || value === dep.replicas} onClick={() => void apply()}>{t("replica.apply")}</Button>
      {added > 0 && <Typography.Text type="secondary" style={{ fontSize: 12 }}>{t("replica.placeOn.hint")}</Typography.Text>}
    </Space>
  );
}

// MultiNodeReplicasDialog edits a multi-node deployment's replica list: each
// replica a leader node and its follower nodes, one per rank. Save replaces
// the list; a replica left out drains and goes, a new one starts once its
// nodes have the model and image.
function MultiNodeReplicasDialog({ dep, ranks, onClose, onChanged }: { dep: Deployment; ranks: number; onClose: () => void; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const [units, setUnits] = useState<string[][]>(() => unitInputs(dep).map((unit) => [unit.node_uuid, ...(unit.followers ?? []).map((rank) => rank.node_uuid)]));
  const { busy, run } = useOrder();
  const [error, setError] = useState<string | null>(null);
  const { t: tDeploy } = useTranslation("deploy");
  const { t: tCommon } = useTranslation("common");
  const [suggesting, setSuggesting] = useState(false);
  const [suggested, setSuggested] = useState<{ type: "info" | "warning"; text: string } | null>(null);
  // Each rank's node is searched on the server; a node that is offline or
  // cordoned is listed but cannot be picked.
  const unpickable = (node: Node) => node.state !== "online" || (node.scheduling ?? "schedulable") !== "schedulable";
  // suggest asks the server for one more replica's nodes given the rows so
  // far, and appends it for the operator to check before saving.
  const suggest = async () => {
    setSuggesting(true);
    setSuggested(null);
    try {
      const listed = units.filter((row) => row.every(Boolean)).map(([leader, ...followers]) => ({ node_uuid: leader, followers: followers.map((node) => ({ node_uuid: node })) }));
      const result = await api.suggestUnit(dep.id, listed);
      if (result.unit) {
        setUnits((current) => [...current, [result.unit!.node_uuid, ...(result.unit!.followers ?? []).map((rank) => rank.node_uuid)]]);
        setSuggested({
          type: "info",
          text: t("replica.suggest.added", { network: result.network?.name ?? "", arch: result.arch ?? "", nodes: (result.nodes ?? []).map((node) => node.name).join(", ") }),
        });
        return;
      }
      const reason = result.reason;
      const params = reason?.params ?? {};
      setSuggested({
        type: "warning",
        text: reason?.code === "unit_no_fit"
          ? t("replica.suggest.noFit", { node: params.node, reason: params.first_reason ? scheduleText(tDeploy, params.first_reason as ScheduleMessage) : "" })
          : t("replica.suggest.none", params),
      });
    } catch (err) {
      setSuggested({ type: "warning", text: t("replica.suggest.failed", { detail: apiErrorText(tCommon, err) }) });
    } finally {
      setSuggesting(false);
    }
  };
  const setRank = (unit: number, rank: number, node: string) =>
    setUnits((current) => current.map((row, index) => (index === unit ? row.map((value, at) => (at === rank ? node : value)) : row)));
  const complete = units.length > 0 && units.every((row) => row.length === ranks && row.every(Boolean));
  const save = async () => {
    setError(null);
    const refused = await run(() =>
      api.scaleReplicas(dep.id, { units: units.map(([leader, ...followers]) => ({ node_uuid: leader, followers: followers.map((node) => ({ node_uuid: node })) })) }),
    );
    if (refused !== null) {
      setError(t("replica.scaleFailed", { detail: refused }));
      return;
    }
    message.success(t("replica.scaled", { count: units.length }));
    onClose();
    onChanged();
  };
  return (
    <Modal
      title={t("replica.unitsTitle")}
      open
      onCancel={() => !busy && onClose()}
      onOk={() => void save()}
      okText={t("replica.save")}
      cancelText={t("detail.cancel")}
      okButtonProps={{ loading: busy, disabled: !complete }}
      cancelButtonProps={{ disabled: busy }}
    >
      <Typography.Paragraph type="secondary">{t("replica.unitsHint", { nnodes: ranks })}</Typography.Paragraph>
      <Space direction="vertical" style={{ width: "100%" }}>
        {units.map((row, unit) => (
          <Space key={unit} wrap align="center">
            <Typography.Text strong>{t("replica.unitLabel", { index: unit + 1 })}</Typography.Text>
            {Array.from({ length: ranks }, (_, rank) => (
              <NodePicker
                key={rank}
                style={{ minWidth: 150 }}
                placeholder={rank === 0 ? t("replica.leaderNode") : t("replica.followerNode", { rank })}
                aria-label={rank === 0 ? t("replica.leaderNodeOf", { index: unit + 1 }) : t("replica.followerNodeOf", { index: unit + 1, rank })}
                value={row[rank] || undefined}
                optionDisabled={unpickable}
                onChange={(node) => setRank(unit, rank, node as string)}
              />
            ))}
            <Button size="small" disabled={units.length <= 1} onClick={() => setUnits((current) => current.filter((_, index) => index !== unit))}>
              {t("replica.removeUnit", { index: unit + 1 })}
            </Button>
          </Space>
        ))}
        <Space wrap>
          <Button size="small" disabled={dep.max_replicas !== undefined && units.length >= dep.max_replicas} onClick={() => setUnits((current) => [...current, Array.from({ length: ranks }, () => "")])}>
            {t("replica.addUnit")}
          </Button>
          <Button size="small" loading={suggesting} disabled={dep.max_replicas !== undefined && units.length >= dep.max_replicas} onClick={() => void suggest()}>
            {t("replica.suggestUnit")}
          </Button>
        </Space>
        {suggested && <Alert type={suggested.type} showIcon message={suggested.text} />}
      </Space>
      {error && <Alert style={{ marginTop: 12 }} type="error" showIcon message={error} />}
    </Modal>
  );
}

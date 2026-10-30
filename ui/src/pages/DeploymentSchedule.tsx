import { useCallback, useEffect, useState } from "react";
import { Alert, Button, Input, InputNumber, Modal, Popconfirm, Select, Space, Switch, Table, Typography, message } from "antd";
import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";
import { api, type Deployment, type LifecycleSchedule, type LifecycleScheduleRequest, type ScheduleAction } from "../api";
import { APIError, apiErrorText } from "../apiError";
import { formatTime } from "../i18n/datetime";

// Monday first, as a week reads; the server counts 0 as Sunday.
const WEEK_ORDER = [1, 2, 3, 4, 5, 6, 0];
const TIME_PATTERN = /^([01]\d|2[0-3]):[0-5]\d$/;
const DEFAULT_MAX_REPLICAS = 32;

// scheduleActions are the orders this deployment may be given: sleep and wake
// need a sleepable deployment, a replica count one whose new replicas get their
// nodes picked: single-node, or multi-node or PD placed automatically (a
// deployment whose replicas the operator placed by hand names the nodes of
// every replica). A PD deployment's count is per role.
export function scheduleActions(dep: Deployment): ScheduleAction[] {
  const actions: ScheduleAction[] = [];
  if (dep.sleepable) actions.push("sleep", "wake");
  const placedByPlatform = dep.pd ? dep.placement_mode === "auto" : (dep.nnodes ?? 1) <= 1 || dep.placement_mode === "auto";
  if (placedByPlatform) actions.push("replicas");
  return actions;
}

// scheduleRoles are the PD roles a replicas order may name, each with the
// fewest replicas it may be given (0 where the mode lets the role be empty).
export function scheduleRoles(dep: Deployment): Array<{ role: string; minimum: number; desired: number }> {
  return (dep.pd?.roles ?? []).map((item) => ({ role: item.role, minimum: item.may_be_empty ? 0 : 1, desired: item.desired }));
}

// useSchedules loads a deployment's standing orders. The scheduling policy
// card reads the same rows for its summary that the editor lists, so the
// editor reloads them here after every change.
export function useSchedules(depID: number) {
  const { t: tCommon } = useTranslation("common");
  const [rows, setRows] = useState<LifecycleSchedule[] | null>(null);
  const [error, setError] = useState("");
  const load = useCallback(async () => {
    try {
      const result = await api.deploymentSchedule(depID);
      setRows(result.items);
      setError("");
    } catch (e) {
      setError(apiErrorText(tCommon, e));
    }
  }, [depID, tCommon]);
  useEffect(() => {
    setRows(null);
    void load();
  }, [load]);
  return { rows, error, load };
}

export type Schedules = ReturnType<typeof useSchedules>;

// scheduleRoleName is a PD role in words ("Prefill"); a role this build does
// not know shows as the server named it.
export function scheduleRoleName(t: TFunction<"deployments">, role: string): string {
  return t(`detail.pd.role.${role}`, { defaultValue: role });
}

// scheduleOrderText words what an order does: "Sleep (L2)", "Wake", "Set
// replicas to 3", "Set Decode replicas to 3" for a PD role.
export function scheduleOrderText(t: TFunction<"deployments">, row: Pick<LifecycleSchedule, "action" | "sleep_level" | "replicas" | "role">): string {
  if (row.action === "sleep") return t("automation.schedule.actionText.sleep", { level: row.sleep_level ?? "" });
  if (row.action === "wake") return t("automation.schedule.actionText.wake");
  return row.role
    ? t("automation.schedule.actionText.roleReplicas", { role: scheduleRoleName(t, row.role), count: row.replicas ?? "" })
    : t("automation.schedule.actionText.replicas", { count: row.replicas ?? "" });
}

// ScheduleSection is a deployment's standing orders: sleep, wake or a replica
// count at a time on some weekdays. Each occurrence runs once, a manual order
// given in between is not overridden, and a new or edited order fires from
// its next occurrence.
export default function ScheduleSection({ dep, schedules }: { dep: Deployment; schedules: Schedules }) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const { rows, error, load } = schedules;
  const [editing, setEditing] = useState<LifecycleSchedule | "new" | null>(null);
  const [busyID, setBusyID] = useState<number | null>(null);
  const actions = scheduleActions(dep);

  const toBody = (row: LifecycleSchedule, enabled: boolean): LifecycleScheduleRequest => ({
    action: row.action, weekdays: row.weekdays, time: row.time, timezone: row.timezone,
    ...(row.action === "sleep" ? { sleep_level: row.sleep_level } : {}),
    ...(row.action === "replicas" ? { replicas: row.replicas, ...(row.role ? { role: row.role } : {}) } : {}),
    enabled,
  });
  const toggle = async (row: LifecycleSchedule, enabled: boolean) => {
    setBusyID(row.id);
    try {
      await api.updateSchedule(dep.id, row.id, toBody(row, enabled));
      await load();
    } catch (e) {
      message.error(t("automation.schedule.saveFailed", { detail: apiErrorText(tCommon, e) }));
    } finally {
      setBusyID(null);
    }
  };
  const remove = async (row: LifecycleSchedule) => {
    setBusyID(row.id);
    try {
      await api.deleteSchedule(dep.id, row.id);
      message.success(t("automation.schedule.deleted"));
      await load();
    } catch (e) {
      message.error(t("automation.schedule.deleteFailed", { detail: apiErrorText(tCommon, e) }));
    } finally {
      setBusyID(null);
    }
  };

  const actionText = (row: Pick<LifecycleSchedule, "action" | "sleep_level" | "replicas" | "role">) => scheduleOrderText(t, row);

  return (
    <section aria-label={t("automation.schedule.title")}>
      <Space style={{ width: "100%", justifyContent: "space-between" }} align="start">
        <Typography.Paragraph type="secondary" style={{ margin: 0, fontSize: 12 }}>{t("automation.schedule.hint")}</Typography.Paragraph>
        <Button size="small" disabled={actions.length === 0} onClick={() => setEditing("new")}>{t("automation.schedule.add")}</Button>
      </Space>
      {actions.length === 0 && (
        <Typography.Paragraph type="secondary" style={{ fontSize: 12, marginTop: 4 }}>{t("automation.schedule.noActions")}</Typography.Paragraph>
      )}
      {error && (
        <Alert
          style={{ marginTop: 8 }}
          type="error"
          showIcon
          message={t("automation.schedule.loadFailed", { detail: error })}
          action={<Button size="small" onClick={() => void load()}>{t("automation.retry")}</Button>}
        />
      )}
      <Table<LifecycleSchedule>
        style={{ marginTop: 8 }}
        size="small"
        rowKey="id"
        loading={rows === null && !error}
        dataSource={rows ?? []}
        pagination={false}
        locale={{ emptyText: error ? "" : t("automation.schedule.empty") }}
        columns={[
          { title: t("automation.schedule.columns.order"), key: "order", render: (_: unknown, row) => actionText(row) },
          { title: t("automation.schedule.columns.days"), key: "days", render: (_: unknown, row) => weekdaysText(t, row.weekdays) },
          {
            title: t("automation.schedule.columns.time"),
            key: "time",
            render: (_: unknown, row) => `${row.time} ${row.timezone || t("automation.schedule.serverZone")}`,
          },
          { title: t("automation.schedule.columns.next"), key: "next", render: (_: unknown, row) => (row.enabled && row.next_at ? formatTime(row.next_at) : "—") },
          {
            title: t("automation.schedule.columns.enabled"),
            key: "enabled",
            render: (_: unknown, row) => (
              <Switch
                size="small"
                checked={row.enabled}
                loading={busyID === row.id}
                aria-label={t("automation.schedule.enabledOf", { order: actionText(row), time: row.time })}
                onChange={(on) => void toggle(row, on)}
              />
            ),
          },
          {
            title: "",
            key: "actions",
            render: (_: unknown, row) => (
              <Space size={4}>
                <Button size="small" onClick={() => setEditing(row)}>{t("automation.schedule.edit")}</Button>
                <Popconfirm
                  title={t("automation.schedule.deleteConfirm")}
                  okText={t("automation.schedule.delete")}
                  cancelText={t("detail.cancel")}
                  okButtonProps={{ danger: true }}
                  onConfirm={() => void remove(row)}
                >
                  <Button size="small" danger loading={busyID === row.id}>{t("automation.schedule.delete")}</Button>
                </Popconfirm>
              </Space>
            ),
          },
        ]}
      />
      {editing && (
        <ScheduleModal
          dep={dep}
          row={editing === "new" ? null : editing}
          actions={actions}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            void load();
          }}
        />
      )}
    </section>
  );
}

function weekdaysText(t: TFunction<"deployments">, days: number[]): string {
  const named = new Set(days);
  if (named.size === 7) return t("automation.schedule.everyDay");
  return WEEK_ORDER.filter((day) => named.has(day)).map((day) => t(`automation.schedule.weekday.${day}`)).join(" ");
}

// ScheduleModal creates or edits one order. The checks it can make itself
// (a day, a time, a count) are made before anything is sent; whatever the
// server still refuses shows above the buttons.
function ScheduleModal({ dep, row, actions, onClose, onSaved }: {
  dep: Deployment;
  row: LifecycleSchedule | null;
  actions: ScheduleAction[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const offered = row && !actions.includes(row.action) ? [row.action, ...actions] : actions;
  const [action, setAction] = useState<ScheduleAction>(row?.action ?? offered[0] ?? "wake");
  const [weekdays, setWeekdays] = useState<number[]>(row?.weekdays ?? [1, 2, 3, 4, 5]);
  const [time, setTime] = useState(row?.time ?? "");
  const [timezone, setTimezone] = useState(row?.timezone ?? "");
  const [level, setLevel] = useState<1 | 2>(row?.sleep_level ?? 2);
  const roles = scheduleRoles(dep);
  const [role, setRole] = useState(row?.role || roles[0]?.role || "");
  const [replicas, setReplicas] = useState<number | null>(row?.replicas ?? (roles.find((item) => item.role === role)?.desired ?? dep.replicas));
  const [enabled, setEnabled] = useState(row?.enabled ?? true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  // A PD role is counted on its own: 1 to 32, or 0 where the mode lets it be empty.
  const minReplicas = roles.find((item) => item.role === role)?.minimum ?? 1;
  const maxReplicas = roles.length > 0 ? DEFAULT_MAX_REPLICAS : (dep.max_replicas ?? DEFAULT_MAX_REPLICAS);

  const invalid = (() => {
    if (weekdays.length === 0) return t("automation.schedule.errors.weekdays");
    if (!TIME_PATTERN.test(time.trim())) return t("automation.schedule.errors.time");
    if (action === "replicas" && (replicas == null || replicas < minReplicas || replicas > maxReplicas)) {
      return t("automation.schedule.errors.replicas", { minimum: minReplicas, maximum: maxReplicas });
    }
    return "";
  })();
  const [showInvalid, setShowInvalid] = useState(false);

  const save = async () => {
    if (invalid) {
      setShowInvalid(true);
      return;
    }
    const body: LifecycleScheduleRequest = {
      action,
      weekdays: [...weekdays].sort((a, b) => a - b),
      time: time.trim(),
      timezone: timezone.trim(),
      ...(action === "sleep" ? { sleep_level: level } : {}),
      ...(action === "replicas" && replicas != null ? { replicas, ...(roles.length > 0 ? { role } : {}) } : {}),
      enabled,
    };
    setSaving(true);
    setError("");
    try {
      if (row) await api.updateSchedule(dep.id, row.id, body);
      else await api.createSchedule(dep.id, body);
      message.success(t("automation.schedule.saved"));
      onSaved();
    } catch (e) {
      setError(scheduleErrorText(e));
    } finally {
      setSaving(false);
    }
  };

  // SCHEDULE_INVALID names the field the server refused; word it by the
  // form's own label for that field.
  const scheduleErrorText = (e: unknown): string => {
    if (e instanceof APIError && e.code === "SCHEDULE_INVALID") {
      const field = String(e.params?.field ?? "");
      return tCommon("apiError.SCHEDULE_INVALID", {
        ...e.params,
        field: t(`automation.schedule.field.${field}`, { defaultValue: field }),
        defaultValue: e.fallback,
      });
    }
    return apiErrorText(tCommon, e);
  };

  return (
    <Modal
      open
      title={row ? t("automation.schedule.editTitle") : t("automation.schedule.addTitle")}
      onCancel={() => !saving && onClose()}
      onOk={() => void save()}
      okText={t("automation.schedule.save")}
      cancelText={t("detail.cancel")}
      okButtonProps={{ loading: saving }}
      cancelButtonProps={{ disabled: saving }}
      maskClosable={!saving}
    >
      <Space direction="vertical" size={12} style={{ width: "100%" }}>
        <Field label={t("automation.schedule.field.action")}>
          <Select<ScheduleAction>
            style={{ width: "100%" }}
            aria-label={t("automation.schedule.field.action")}
            value={action}
            onChange={setAction}
            options={offered.map((value) => ({ value, label: t(`automation.schedule.actionName.${value}`) }))}
          />
        </Field>
        <Field label={t("automation.schedule.field.weekdays")}>
          <Space size={4} wrap role="group" aria-label={t("automation.schedule.field.weekdays")}>
            {WEEK_ORDER.map((day) => {
              const on = weekdays.includes(day);
              return (
                <Button
                  key={day}
                  size="small"
                  type={on ? "primary" : "default"}
                  aria-pressed={on}
                  onClick={() => setWeekdays((current) => (on ? current.filter((value) => value !== day) : [...current, day]))}
                >
                  {t(`automation.schedule.weekday.${day}`)}
                </Button>
              );
            })}
          </Space>
        </Field>
        <Field label={t("automation.schedule.field.time")}>
          <Input
            type="time"
            style={{ width: 140 }}
            aria-label={t("automation.schedule.field.time")}
            value={time}
            onChange={(event) => setTime(event.target.value)}
          />
        </Field>
        <Field label={t("automation.schedule.field.timezone")} hint={t("automation.schedule.timezoneHint")}>
          <Input
            aria-label={t("automation.schedule.field.timezone")}
            placeholder={t("automation.schedule.serverZone")}
            value={timezone}
            onChange={(event) => setTimezone(event.target.value)}
          />
        </Field>
        {action === "sleep" && (
          <Field label={t("automation.schedule.field.sleep_level")}>
            <Select<1 | 2>
              style={{ width: "100%" }}
              aria-label={t("automation.schedule.field.sleep_level")}
              value={level}
              onChange={setLevel}
              options={[
                { value: 1, label: t("automation.level.1") },
                { value: 2, label: t("automation.level.2") },
              ]}
            />
          </Field>
        )}
        {action === "replicas" && roles.length > 0 && (
          <Field label={t("automation.schedule.field.role")} hint={t("automation.schedule.roleHint")}>
            <Select<string>
              style={{ width: "100%" }}
              aria-label={t("automation.schedule.field.role")}
              value={role}
              onChange={setRole}
              options={roles.map((item) => ({ value: item.role, label: scheduleRoleName(t, item.role) }))}
            />
          </Field>
        )}
        {action === "replicas" && (
          <Field label={t("automation.schedule.field.replicas")}>
            <InputNumber
              min={minReplicas}
              max={maxReplicas}
              precision={0}
              aria-label={t("automation.schedule.field.replicas")}
              value={replicas}
              onChange={setReplicas}
            />
          </Field>
        )}
        <Space size={8}>
          <Switch checked={enabled} onChange={setEnabled} aria-label={t("automation.schedule.field.enabled")} />
          <Typography.Text>{t("automation.schedule.field.enabled")}</Typography.Text>
        </Space>
        {showInvalid && invalid && <Alert type="warning" showIcon message={invalid} />}
        {error && <Alert type="error" showIcon message={error} />}
      </Space>
    </Modal>
  );
}

function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <div>
      <Typography.Text strong style={{ display: "block", marginBottom: 4 }}>{label}</Typography.Text>
      {children}
      {hint && <Typography.Text type="secondary" style={{ display: "block", fontSize: 12, marginTop: 2 }}>{hint}</Typography.Text>}
    </div>
  );
}

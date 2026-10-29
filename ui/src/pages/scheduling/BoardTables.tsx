import { Button } from "antd";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { BoardPending, BoardUpcoming, BoardWaitingWake, ScheduleMessage } from "../../api";
import ResourceTable, { type ResourceColumn } from "../../components/resourceTable/ResourceTable";
import { formatTime } from "../../i18n/datetime";
import { reasonWithFirst } from "../DeploymentPlacementStatus";
import { scheduleText } from "../deploy/deploySchedule";

function DeploymentLink({ id, name }: { id: number; name: string }) {
  return <Link to={`/deployments?deployment=${id}`}>{name}</Link>;
}

// Each table is small and fully loaded with the board, so none pages or
// remembers a column layout.
function BoardTable<T extends object>({ id, columns, data, rowKey, empty }: { id: string; columns: ResourceColumn<T>[]; data: T[]; rowKey: (row: T) => string; empty: string }) {
  return <ResourceTable<T> tableId={id} columns={columns} data={data} rowKey={rowKey} pagination={false} columnSettings={false} emptyDescription={empty} />;
}

// PendingTable lists deployments the controller could not place, worded the
// way a deployment's own placement alert words them.
export function PendingTable({ rows }: { rows: BoardPending[] }) {
  const { t } = useTranslation("scheduling");
  const { t: tDeploy } = useTranslation("deploy");
  const render = (value: ScheduleMessage) => scheduleText(tDeploy, value);
  const columns: ResourceColumn<BoardPending>[] = [
    { key: "deployment", title: t("pending.columns.deployment"), width: 200, render: (_: unknown, row) => <DeploymentLink id={row.deployment_id} name={row.deployment} /> },
    { key: "reason", title: t("pending.columns.reason"), width: 480, ellipsis: false, render: (_: unknown, row) => render(reasonWithFirst(row.reason, render)) || "—" },
    { key: "at", title: t("pending.columns.at"), width: 160, render: (_: unknown, row) => formatTime(row.at) },
  ];
  return <BoardTable id="scheduling-pending" columns={columns} data={rows} rowKey={(row) => String(row.deployment_id)} empty={t("pending.empty")} />;
}

// WaitingWakesTable lists wakes that wait for room; each can be given up.
export function WaitingWakesTable({ rows, busyID, onCancel }: { rows: BoardWaitingWake[]; busyID: number | null; onCancel: (row: BoardWaitingWake) => void }) {
  const { t } = useTranslation("scheduling");
  const columns: ResourceColumn<BoardWaitingWake>[] = [
    { key: "deployment", title: t("waiting.columns.deployment"), width: 200, render: (_: unknown, row) => <DeploymentLink id={row.deployment_id} name={row.deployment} /> },
    { key: "instances", title: t("waiting.columns.instances"), width: 260, render: (_: unknown, row) => row.instances.map((id) => id.slice(0, 8)).join(", ") },
    { key: "since", title: t("waiting.columns.since"), width: 160, render: (_: unknown, row) => formatTime(row.since) },
    {
      key: "actions",
      role: "actions",
      width: 160,
      render: (_: unknown, row) => (
        <Button size="small" loading={busyID === row.deployment_id} aria-label={t("waiting.cancelOf", { name: row.deployment })} onClick={() => onCancel(row)}>
          {t("waiting.cancel")}
        </Button>
      ),
    },
  ];
  return <BoardTable id="scheduling-waiting" columns={columns} data={rows} rowKey={(row) => String(row.deployment_id)} empty={t("waiting.empty")} />;
}

// UpcomingTable lists what the schedules will do in the next 24 hours.
export function UpcomingTable({ rows }: { rows: BoardUpcoming[] }) {
  const { t } = useTranslation("scheduling");
  const { t: tDeployments } = useTranslation("deployments");
  const columns: ResourceColumn<BoardUpcoming>[] = [
    { key: "at", title: t("upcoming.columns.at"), width: 160, render: (_: unknown, row) => formatTime(row.at) },
    { key: "deployment", title: t("upcoming.columns.deployment"), width: 200, render: (_: unknown, row) => <DeploymentLink id={row.deployment_id} name={row.deployment} /> },
    {
      key: "action",
      title: t("upcoming.columns.action"),
      width: 220,
      render: (_: unknown, row) => {
        const key = row.action === "replicas" && row.role ? "roleReplicas" : row.action;
        return t(`upcoming.action.${key}`, { level: row.sleep_level ?? "", count: row.replicas ?? "", role: row.role ? tDeployments(`detail.pd.role.${row.role}`, { defaultValue: row.role }) : "", defaultValue: row.action });
      },
    },
  ];
  return <BoardTable id="scheduling-upcoming" columns={columns} data={rows} rowKey={(row) => `${row.schedule_id}-${row.at}`} empty={t("upcoming.empty")} />;
}

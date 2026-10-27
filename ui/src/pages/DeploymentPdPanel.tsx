import { useState } from "react";
import { Badge, Button, Space, Tag, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { api, type Deployment, type DeploymentPd } from "../api";
import DeploymentPdScaleModal from "./DeploymentPdScaleModal";
import { UnitRetryAlert } from "./OrderDialogs";

// pdStateBadge maps the derived PD state (never persisted, see ADR 0028) to
// the same Badge status vocabulary the ordinary deployment state column uses.
function pdStateBadge(state: DeploymentPd["state"]): "success" | "warning" | "default" {
  if (state === "running") return "success";
  if (state === "partial") return "warning";
  return "default";
}

// PdSummary renders a PD deployment's mode, derived state, the "some role is
// short" notice, and each role's TP/ready/desired - the fields deployment.pd
// carries (ADR 0028) - plus the "Scale roles" action that opens
// DeploymentPdScaleModal. Restart is the drawer header's, for every shape.
function PdSummary({ pd, onScale }: { pd: DeploymentPd; onScale: () => void }) {
  const { t } = useTranslation("deployments");
  return (
    <div style={{ marginTop: 16 }}>
      <Space align="center" style={{ width: "100%", justifyContent: "space-between" }} wrap>
        <Typography.Title level={5} style={{ margin: 0 }}>
          {t("detail.pd.title")}
        </Typography.Title>
        <Button size="small" onClick={onScale}>
          {t("detail.pd.scale.action")}
        </Button>
      </Space>
      <Space wrap style={{ marginTop: 8 }}>
        <Tag color="blue">{t("detail.pd.mode", { mode: pd.mode })}</Tag>
        <Badge status={pdStateBadge(pd.state)} text={t(`detail.pd.state.${pd.state}`)} />
        {pd.short && <Tag color="orange">{t("detail.pd.short")}</Tag>}
      </Space>
      <Space wrap style={{ marginTop: 8 }}>
        {pd.roles.map((role) => (
          <Tag key={role.role}>
            {t("detail.pd.roleSummary", {
              role: t(`detail.pd.role.${role.role}`),
              tp: role.tp,
              ready: role.ready,
              desired: role.desired,
            })}
          </Tag>
        ))}
      </Space>
    </div>
  );
}

// PdExhaustedUnits lists the units whose retry budget is spent
// (retry_exhausted, derived by the server from the controller's give-up
// record) with the newest failure and a Retry, which gives that one unit its
// budget back without touching the others (ADR 0029).
function PdExhaustedUnits({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const units = (dep.pd?.roles ?? []).flatMap((role) =>
    role.units.map((unit, index) => ({ role: role.role, index, unit })).filter((item) => item.unit.retry_exhausted),
  );
  return (
    <>
      {units.map(({ role, index, unit }) => (
        <UnitRetryAlert
          key={`${role}/${index}`}
          message={t("detail.pd.retry.exhausted", {
            role: t(`detail.pd.role.${role}`),
            index: index + 1,
            node: unit.node_name || unit.node_uuid,
            attempts: unit.failed_attempts ?? 0,
            reason: unit.last_failure ?? "",
          })}
          retry={() => api.retryDeploymentUnit(dep.id, { role, index })}
          onDone={onChanged}
          editHref={`/deploy?edit=${dep.id}`}
        />
      ))}
    </>
  );
}

// DeploymentPdPanel is the whole PD section of the deployment detail drawer:
// mode/state/short summary with its "Scale roles" action, the units that gave
// up with their Retry action, and the scale modal. An ordinary deployment has
// no dep.pd and never mounts this (ADR 0028).
export default function DeploymentPdPanel({
  dep,
  onChanged,
}: {
  dep: Deployment;
  onChanged: () => void;
}) {
  const [scaleOpen, setScaleOpen] = useState(false);
  if (!dep.pd) return null;
  return (
    <>
      <PdSummary pd={dep.pd} onScale={() => setScaleOpen(true)} />
      <PdExhaustedUnits dep={dep} onChanged={onChanged} />
      <DeploymentPdScaleModal
        open={scaleOpen}
        dep={dep}
        onClose={() => setScaleOpen(false)}
        onChanged={onChanged}
      />
    </>
  );
}

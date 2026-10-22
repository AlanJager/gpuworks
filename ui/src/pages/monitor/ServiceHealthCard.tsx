import { useMemo } from "react";
import { Badge, Tag, Tooltip } from "antd";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import type { HealthProbe, MonitorDeployment } from "../../api";
import {
  HealthTag,
  PROBE_COLOR,
  ProbeLines,
} from "../../components/DeploymentHealth";
import { colors } from "../../theme/tokens";
import InfoTitle from "./InfoTitle";
import { formatMS } from "./requestShape";
import { replicaLabels } from "./RequestView";

const STATE_COLOR: Record<string, string> = {
  running: "green",
  starting: "blue",
  stopped: "default",
  failed: "red",
  asleep: "purple",
};

const RECENT = 10;

// ServiceHealthCard sits beside the latency breakdown: what the health
// check found (its status and the recent checks as a strip, each slot's
// details in its tooltip), then how many replicas run against how many the
// deployment wants and each replica's state, all in two compact lines.
export default function ServiceHealthCard({
  data,
}: {
  data: MonitorDeployment;
}) {
  const { t } = useTranslation("monitoring");
  const { t: tCommon } = useTranslation("common");
  const { ready, desired } = data.deployment.replicas;
  const labels = useMemo(() => replicaLabels(data.replicas), [data.replicas]);
  const replicaName = (uuid: string) => {
    const label = labels.get(uuid);
    return label
      ? t("requests.replica", { n: label.n, node: label.node })
      : undefined;
  };
  const health = data.health;
  const slots: (HealthProbe | null)[] = health
    ? [
        ...Array<null>(Math.max(0, RECENT - health.recent.length)).fill(null),
        ...health.recent,
      ]
    : [];
  return (
    <div className="monitor-health">
      <InfoTitle title={t("health.title")} help={t("health.help")} />
      <div className="monitor-health-line">
        <HealthTag health={health} />
        {health?.status === "off" && (
          <Link to={`/deployments?deployment=${data.deployment.id}`}>
            {t("health.enable")}
          </Link>
        )}
        {health && health.status !== "off" && (
          <div
            className="monitor-health-strip"
            role="list"
            aria-label={t("health.recent")}
          >
            {slots.map((probe, i) =>
              probe ? (
                <Tooltip
                  key={probe.at_ms}
                  title={
                    <div
                      style={{
                        display: "flex",
                        flexDirection: "column",
                        gap: 2,
                      }}
                    >
                      <ProbeLines probe={probe} replicaName={replicaName} />
                    </div>
                  }
                  trigger={["hover", "focus"]}
                >
                  <span
                    role="listitem"
                    tabIndex={0}
                    data-status={probe.status}
                    aria-label={`${tCommon(`healthCheck.status.${probe.status}`)} ${formatMS(probe.latency_ms)}`}
                    style={{ background: PROBE_COLOR[probe.status] }}
                  />
                </Tooltip>
              ) : (
                <span
                  key={`empty-${i}`}
                  aria-hidden
                  style={{ background: colors.borderSoft }}
                />
              ),
            )}
          </div>
        )}
      </div>
      <div className="monitor-health-replicas">
        <Badge status={ready < desired ? "warning" : "success"} />
        <span>{t("health.replicasReady", { ready, desired })}</span>
        {data.replicas.map((replica, i) => (
          <Tag
            key={replica.instance_uuid}
            color={STATE_COLOR[replica.state] ?? "default"}
          >
            {t("requests.replica", { n: i + 1, node: replica.node_name })} ·{" "}
            {t(`states.${replica.state}`, { defaultValue: replica.state })}
          </Tag>
        ))}
      </div>
    </div>
  );
}

import { useState } from "react";
import { InfoCircleOutlined } from "@ant-design/icons";
import { Button, Card, Modal, Space, Tabs, Tooltip, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { Deployment } from "../api";
import { useIsAdmin } from "../components/useIsAdmin";
import { useSchedules } from "./DeploymentSchedule";
import { policySections, policySummary } from "./schedulingPolicySections";

// DeploymentSchedulingPolicy is what the platform does to a deployment on its
// own, in one card: where its replicas go when their node is lost, its standing
// schedule and its yield policy. Collapsed it is one line of what each is set
// to; editing opens the editors, one tab per policy.
export default function DeploymentSchedulingPolicy({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  if (dep.desired_state === "deleting") return null;
  return <PolicyCard dep={dep} onChanged={onChanged} />;
}

function PolicyCard({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const isAdmin = useIsAdmin();
  const schedules = useSchedules(dep.id);
  const [editing, setEditing] = useState(false);
  const context = { dep, schedules, onChanged };
  const sections = policySections.filter((section) => section.visible(dep));
  return (
    <Card
      size="small"
      style={{ marginTop: 20 }}
      role="region"
      aria-label={t("schedulingPolicy.title")}
      title={
        <Space size={4}>
          {t("schedulingPolicy.title")}
          <Tooltip title={<div style={{ maxWidth: 360 }}>{t("schedulingPolicy.help")}</div>} trigger={["hover", "focus"]}>
            <InfoCircleOutlined role="img" aria-label={t("schedulingPolicy.helpLabel")} tabIndex={0} style={{ fontSize: 13, cursor: "help" }} />
          </Tooltip>
        </Space>
      }
      extra={isAdmin && <Button size="small" onClick={() => setEditing(true)}>{t("schedulingPolicy.edit")}</Button>}
    >
      <Typography.Text>{policySummary(t, context)}</Typography.Text>
      {editing && (
        <Modal
          open
          width={720}
          title={t("schedulingPolicy.title")}
          onCancel={() => setEditing(false)}
          footer={<Button onClick={() => setEditing(false)}>{t("schedulingPolicy.close")}</Button>}
        >
          <Tabs
            items={sections.map((section) => ({ key: section.key, label: t(section.label), children: section.render(context) }))}
          />
        </Modal>
      )}
    </Card>
  );
}

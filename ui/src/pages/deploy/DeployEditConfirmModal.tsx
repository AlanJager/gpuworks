import { useEffect, useState } from "react";
import { Alert, Button, Checkbox, Modal, Radio, Space, Table, Tag, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { DeploymentChange, DeploymentEditApply, DeploymentEditPlan } from "../../api";
import { APIError, apiErrorText } from "../../apiError";
import type { EditChoice, EditReview } from "./useDeployEditSubmit";

// The way an edit reaches the instances, as the radio group offers it: the
// server's apply options, and waking a sleeping deployment, which the server
// answers with apply all plus wake.
type Selection = DeploymentEditApply | "wake";

const MASK = "••••";
const NONE = "—";

function shown(change: DeploymentChange, value: string): string {
  if (change.masked) return MASK;
  return value === "" ? NONE : value;
}

function choiceOf(selection: Selection, start: boolean): EditChoice {
  if (selection === "wake") return { apply: "all", wake: true };
  return { apply: selection, ...(start ? { start: true } : {}) };
}

function okKey(plan: DeploymentEditPlan, selection: Selection, start: boolean): string {
  if (!plan.restart) return start ? "edit.ok.startSave" : "edit.ok.save";
  if (selection === "rolling") return "edit.ok.rolling";
  if (selection === "all") return "edit.ok.all";
  if (selection === "wake") return "edit.ok.wake";
  return start ? "edit.ok.startSave" : "edit.ok.save";
}

// DeployEditConfirmModal is the last step of an edit: what the server will
// change (each field with its old and new value, and whether it takes effect
// at once or with replacement instances), how the change reaches the running
// instances, and the buttons that commit it. An error the save met stays here
// beside the choice that met it; a deployment that moved on offers a reload
// that keeps the deployer's edits. It is mounted while a review is open and
// leaves with it.
export default function DeployEditConfirmModal({
  review,
  saving,
  reloading,
  onCancel,
  onSave,
  onReload,
}: {
  review: EditReview;
  saving: boolean;
  reloading: boolean;
  onCancel: () => void;
  onSave: (choice: EditChoice) => void;
  onReload: () => void;
}) {
  const { t } = useTranslation("deploy");
  const { t: tCommon } = useTranslation("common");
  const plan = review.plan;
  const [selection, setSelection] = useState<Selection>("none");
  const [start, setStart] = useState(false);
  useEffect(() => {
    setSelection(plan?.default_apply ?? "none");
    setStart(false);
  }, [plan]);

  const conflict = review.error instanceof APIError && review.error.code === "DEPLOYMENT_CHANGED";
  const options: Selection[] = plan
    ? [...plan.apply_options, ...(plan.can_wake ? (["wake"] as const) : [])]
    : [];
  const askApply = Boolean(plan?.restart) && options.length > 0;
  return (
    <Modal
      open
      title={t("edit.confirm.title")}
      width={720}
      onCancel={saving ? undefined : onCancel}
      maskClosable={!saving}
      keyboard={!saving}
      footer={
        <Space>
          <Button onClick={onCancel} disabled={saving}>
            {t("edit.confirm.cancel")}
          </Button>
          {plan && (
            <Button
              type="primary"
              loading={saving}
              disabled={conflict || plan.changes.length === 0}
              onClick={() => onSave(askApply ? choiceOf(selection, start) : start ? { start: true } : {})}
            >
              {t(okKey(plan, askApply ? selection : "none", start))}
            </Button>
          )}
        </Space>
      }
    >
      {review.error != null && (
        <Alert
          style={{ marginBottom: 12 }}
          type="error"
          showIcon
          message={conflict ? t("edit.confirm.conflict") : apiErrorText(tCommon, review.error)}
          description={conflict ? t("edit.confirm.conflictHelp") : undefined}
          action={
            conflict ? (
              <Button size="small" loading={reloading} onClick={onReload}>
                {t("edit.confirm.reload")}
              </Button>
            ) : undefined
          }
        />
      )}
      {plan && (
        <>
          {plan.changes.length === 0 ? (
            <Typography.Paragraph type="secondary">{t("edit.confirm.noChanges")}</Typography.Paragraph>
          ) : (
            <Table<DeploymentChange>
              size="small"
              pagination={false}
              rowKey="field"
              dataSource={plan.changes}
              columns={[
                {
                  title: t("edit.confirm.field"),
                  dataIndex: "field",
                  render: (field: string) => <Typography.Text code>{field}</Typography.Text>,
                },
                {
                  title: t("edit.confirm.from"),
                  render: (_: unknown, change) => <code>{shown(change, change.from)}</code>,
                },
                {
                  title: t("edit.confirm.to"),
                  render: (_: unknown, change) => <code>{shown(change, change.to)}</code>,
                },
                {
                  title: t("edit.confirm.layer"),
                  dataIndex: "layer",
                  render: (layer: DeploymentChange["layer"]) => (
                    <Tag color={layer === "live" ? "green" : "orange"}>
                      {t(`edit.layer.${layer}`)}
                    </Tag>
                  ),
                },
              ]}
            />
          )}
          {askApply && (
            <div style={{ marginTop: 16 }}>
              <Typography.Text strong>{t("edit.confirm.apply")}</Typography.Text>
              {plan.apply_note && (
                <Alert style={{ marginTop: 8 }} type="warning" showIcon message={t(`edit.applyNote.${plan.apply_note}`)} />
              )}
              <Radio.Group
                style={{ display: "block", marginTop: 8 }}
                value={selection}
                onChange={(event) => setSelection(event.target.value as Selection)}
              >
                <Space direction="vertical">
                  {options.map((option) => (
                    <Radio key={option} value={option}>
                      {t(`edit.apply.${option}.label`)}
                      <Typography.Text type="secondary"> — {t(`edit.apply.${option}.help`)}</Typography.Text>
                    </Radio>
                  ))}
                </Space>
              </Radio.Group>
            </div>
          )}
          {plan.can_start && selection !== "wake" && (
            <Checkbox style={{ marginTop: 12 }} checked={start} onChange={(event) => setStart(event.target.checked)}>
              {t("edit.confirm.startAfter")}
            </Checkbox>
          )}
        </>
      )}
    </Modal>
  );
}

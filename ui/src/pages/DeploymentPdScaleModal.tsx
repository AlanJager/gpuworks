import { useEffect, useState } from "react";
import { Alert, Button, Form, Modal, Select, Space, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type Deployment, type PdModeProfile, type PdRoleName, type PdScaleFollowerInput, type PdScaleUnitInput } from "../api";
import { PD_ROLE_ORDER, pdModeOf, pdRoleRemovable } from "./deploy/deployPd";
import PdUnitRow, { type PdNodeChoice } from "./deploy/PdUnitFields";
import { apiErrorText } from "../apiError";

interface FollowerDraft {
  node_uuid?: string;
  gpus?: number[];
}

interface UnitDraft {
  node_uuid?: string;
  gpus?: number[];
  followers?: FollowerDraft[];
}

type RolesDraft = Partial<Record<PdRoleName, { units: UnitDraft[] }>>;

interface ScaleFormValues {
  roles?: RolesDraft;
  fallback?: string;
}

// initialRoles seeds the form from the deployment's current units, followers
// included - exactly what the drawer already shows, so opening the modal
// never appears to silently rearrange anything.
function initialRoles(dep: Deployment): RolesDraft {
  const roles: RolesDraft = {};
  for (const role of dep.pd?.roles ?? []) {
    roles[role.role] = {
      units: role.units.map((unit) => ({
        node_uuid: unit.node_uuid,
        gpus: unit.gpus,
        followers: unit.followers?.map((follower) => ({ node_uuid: follower.node_uuid, gpus: follower.gpus })),
      })),
    };
  }
  return roles;
}

// DeploymentPdScaleModal edits a PD deployment's per-role unit list and
// fallback policy via PUT /deployments/{id}/pd (ADR 0028). Mode, TP and the
// KV load failure policy are fixed once instances start (PD_IMMUTABLE) - the
// form never renders or sends them, only `roles.<role>.units` and
// `fallback`. Which roles may lose their last unit, the unit ceiling and the
// fallback options come from the mode's entry in the engine catalog; until it
// has loaded every role keeps one unit and the fallback stays as it is. The
// server checks all of it again.
export default function DeploymentPdScaleModal({
  open,
  dep,
  onClose,
  onChanged,
}: {
  open: boolean;
  dep: Deployment;
  onClose: () => void;
  onChanged: () => void;
}) {
  const { t } = useTranslation("deployments");
  const { t: tDeploy } = useTranslation("deploy");
  const { t: tCommon } = useTranslation("common");
  const [form] = Form.useForm<ScaleFormValues>();
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [mode, setMode] = useState<PdModeProfile>();
  // No evaluate result exists for an already-running deployment, so every
  // online node is offered enabled (PdNodeChoice without eligibility) - the
  // server, not this form, judges whether a pick still fits.
  const choice: PdNodeChoice = {};

  useEffect(() => {
    if (!open) return;
    form.setFieldsValue({
      roles: initialRoles(dep),
      fallback: dep.pd?.fallback,
    });
    setError(null);
  }, [open, dep, form]);

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    api
      .engines()
      .then((profiles) => {
        if (!cancelled) setMode(pdModeOf(profiles.items.find((profile) => profile.name === dep.engine), dep.pd?.mode));
      })
      .catch(() => {
        // Without the catalog the dialog keeps its conservative defaults.
      });
    return () => {
      cancelled = true;
    };
  }, [open, dep.engine, dep.pd?.mode]);

  const fallbackOptions = mode?.fallbacks ?? (dep.pd?.fallback ? [dep.pd.fallback] : []);

  const roleTP = (role: PdRoleName) => dep.pd?.roles.find((item) => item.role === role)?.tp ?? 1;

  const submit = async () => {
    let values: ScaleFormValues;
    try {
      values = await form.validateFields();
    } catch {
      return;
    }
    const roles: Partial<Record<PdRoleName, { units: PdScaleUnitInput[] }>> = {};
    for (const role of PD_ROLE_ORDER) {
      const draft = values.roles?.[role];
      if (!draft) continue;
      roles[role] = {
        units: (draft.units ?? [])
          .filter((unit): unit is UnitDraft & { node_uuid: string } => Boolean(unit.node_uuid))
          .map((unit) => {
            const out: PdScaleUnitInput = { node_uuid: unit.node_uuid };
            if (unit.gpus?.length) out.gpus = unit.gpus;
            const followers = (unit.followers ?? [])
              .filter((follower): follower is FollowerDraft & { node_uuid: string } => Boolean(follower.node_uuid))
              .map((follower): PdScaleFollowerInput =>
                follower.gpus?.length ? { node_uuid: follower.node_uuid, gpus: follower.gpus } : { node_uuid: follower.node_uuid },
              );
            if (followers.length) out.followers = followers;
            return out;
          }),
      };
    }
    setSaving(true);
    setError(null);
    try {
      await api.updateDeploymentPd(dep.id, { roles, fallback: values.fallback });
      message.success(t("detail.pd.scale.saved"));
      onClose();
      onChanged();
    } catch (e) {
      setError(t("detail.pd.scale.failed", { detail: apiErrorText(tCommon, e) }));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      title={t("detail.pd.scale.title")}
      open={open}
      onCancel={() => !saving && onClose()}
      onOk={() => void submit()}
      okText={t("detail.pd.scale.save")}
      cancelText={t("detail.pd.scale.cancel")}
      confirmLoading={saving}
      maskClosable={!saving}
      closable={!saving}
      destroyOnClose
    >
      {error && <Alert type="error" showIcon message={error} style={{ marginBottom: 12 }} />}
      <Form form={form} layout="vertical">
        {PD_ROLE_ORDER.map((role) => (
          <div key={role} className="deploy-node-card" style={{ marginBottom: 12 }}>
            <div className="deploy-node-card-head">
              <strong>{tDeploy(`pd.role.${role}`)}</strong>
              <span>{t("detail.pd.scale.tp", { tp: roleTP(role) })}</span>
            </div>
            <Form.List name={["roles", role, "units"]}>
              {(fields, { add, remove }) => (
                <Space direction="vertical" style={{ width: "100%" }} size={6}>
                  {fields.map((field, index) => (
                    <PdUnitRow
                      key={field.key}
                      role={role}
                      index={index}
                      rootName={["roles", role, "units"]}
                      fieldName={field.name}
                      choice={choice}
                      removable={pdRoleRemovable(mode, role, fields.length)}
                      onRemove={() => remove(field.name)}
                    />
                  ))}
                  <Button
                    type="dashed"
                    size="small"
                    disabled={mode != null && fields.length >= mode.max_units_per_role}
                    onClick={() => add({})}
                  >
                    {t("detail.pd.scale.addUnit", { role: tDeploy(`pd.role.${role}`) })}
                  </Button>
                </Space>
              )}
            </Form.List>
          </div>
        ))}
        <Form.Item label={t("detail.pd.scale.fallback")} name="fallback">
          <Select
            options={fallbackOptions.map((value) => ({
              value,
              label: tDeploy(`pd.fallbackOption.${value}`, { defaultValue: value }),
            }))}
          />
        </Form.Item>
      </Form>
    </Modal>
  );
}

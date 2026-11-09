import { useEffect, useRef, useState } from "react";
import { Form, Modal, Radio, Select, Space, Tag, Tooltip, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type ApiKey, type ApiKeyScope, type Deployment } from "../../api";

// The deployments a key may be limited to, loaded once per picker. A failed
// load keeps "all model services" usable and says why the list is empty.
function useDeploymentOptions() {
  const [deployments, setDeployments] = useState<Deployment[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    let live = true;
    api
      .deployments()
      .then((list) => live && setDeployments(list.items))
      .catch((e: Error) => live && setError(e.message))
      .finally(() => live && setLoading(false));
    return () => {
      live = false;
    };
  }, []);
  return { deployments, loading, error };
}

export const ALL_DEPLOYMENTS: ApiKeyScope = { all_deployments: true, deployment_ids: [] };

/** KeyScopeInput is a Form control choosing all model services or some. */
export function KeyScopeInput({ value = ALL_DEPLOYMENTS, onChange }: { value?: ApiKeyScope; onChange?: (next: ApiKeyScope) => void }) {
  const { t } = useTranslation("apikeys");
  const { deployments, loading, error } = useDeploymentOptions();
  return (
    <Space direction="vertical" style={{ width: "100%" }}>
      <Radio.Group
        value={value.all_deployments ? "all" : "some"}
        onChange={(e) => onChange?.({ all_deployments: e.target.value === "all", deployment_ids: value.deployment_ids })}
      >
        <Radio value="all">{t("scope.all")}</Radio>
        <Radio value="some">{t("scope.some")}</Radio>
      </Radio.Group>
      {!value.all_deployments && (
        <>
          <Select
            mode="multiple"
            allowClear
            showSearch
            optionFilterProp="label"
            aria-label={t("scope.pick")}
            placeholder={t("scope.pick")}
            loading={loading}
            value={value.deployment_ids}
            onChange={(ids: number[]) => onChange?.({ all_deployments: false, deployment_ids: ids })}
            options={deployments.map((d) => ({ value: d.id, label: d.served_name && d.served_name !== d.name ? `${d.name} (${d.served_name})` : d.name }))}
            notFoundContent={loading ? undefined : t("scope.noDeployments")}
          />
          {error && <Typography.Text type="danger">{t("scope.loadFailed", { detail: error })}</Typography.Text>}
        </>
      )}
    </Space>
  );
}

/**
 * scopeRequest is the scope as the server takes it. The picker keeps the
 * chosen services while "all" is selected, so switching back restores them;
 * they are not sent with "all", which the server refuses as contradictory.
 */
export function scopeRequest(v: ApiKeyScope = ALL_DEPLOYMENTS): ApiKeyScope {
  return v.all_deployments ? ALL_DEPLOYMENTS : v;
}

/** scopeRule asks for at least one service when the key is limited. */
export function scopeRule(t: (key: string) => string) {
  return {
    validator: (_: unknown, v?: ApiKeyScope) =>
      !v || v.all_deployments || v.deployment_ids.length > 0 ? Promise.resolve() : Promise.reject(new Error(t("scope.required"))),
  };
}

/** KeyScopeCell shows what a key may call in the key list. */
export function KeyScopeCell({ apiKey }: { apiKey: ApiKey }) {
  const { t } = useTranslation("apikeys");
  if (apiKey.all_deployments) return <Tag>{t("scope.all")}</Tag>;
  if (!apiKey.deployments.length) {
    return (
      <Tooltip title={t("scope.noneLeftHelp")}>
        <Tag color="red">{t("scope.noneLeft")}</Tag>
      </Tooltip>
    );
  }
  const shown = apiKey.deployments.slice(0, 2);
  const rest = apiKey.deployments.slice(2);
  return (
    <Space size={4} wrap>
      {shown.map((d) => (
        <Tag key={d.id} color="blue">{d.served_name || d.name}</Tag>
      ))}
      {rest.length > 0 && (
        <Tooltip title={rest.map((d) => d.served_name || d.name).join(", ")}>
          <Tag>{t("scope.more", { count: rest.length })}</Tag>
        </Tooltip>
      )}
    </Space>
  );
}

/** KeyScopeModal edits a live key's scope; it applies on the key's next request. */
export function KeyScopeModal({ apiKey, onClose, onSaved }: { apiKey: ApiKey | null; onClose: () => void; onSaved: () => void }) {
  const { t } = useTranslation("apikeys");
  const [form] = Form.useForm<{ scope: ApiKeyScope }>();
  const [saving, setSaving] = useState(false);
  const savingRef = useRef(false);

  const save = async () => {
    if (!apiKey || savingRef.current) return;
    savingRef.current = true;
    try {
      let v;
      try {
        v = await form.validateFields();
      } catch {
        return;
      }
      setSaving(true);
      await api.setApiKeyScope(apiKey.prefix, scopeRequest(v.scope));
      message.success(t("scope.saved"));
      onSaved();
    } catch (e) {
      message.error(t("scope.saveFailed", { detail: (e as Error).message }));
    } finally {
      savingRef.current = false;
      setSaving(false);
    }
  };

  return (
    <Modal
      title={t("scope.editTitle", { name: apiKey?.name ?? "" })}
      open={!!apiKey}
      onCancel={onClose}
      onOk={save}
      confirmLoading={saving}
      okText={t("actions.save")}
      cancelText={t("actions.cancel")}
      destroyOnHidden
    >
      {apiKey && (
        <Form
          form={form}
          layout="vertical"
          preserve={false}
          initialValues={{ scope: { all_deployments: apiKey.all_deployments, deployment_ids: apiKey.deployments.map((d) => d.id) } }}
        >
          <Form.Item label={t("scope.label")} name="scope" rules={[scopeRule(t)]} extra={t("scope.editHelp")}>
            <KeyScopeInput />
          </Form.Item>
        </Form>
      )}
    </Modal>
  );
}

import { useRef, useState } from "react";
import { Alert, Button, Card, Form, Input, Modal, Popconfirm, Radio, Select, Tag, Tooltip, Typography, message } from "antd";
import { DeleteOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { api, usePolling, type List, type PlatformToken } from "../../api";
import { apiErrorText } from "../../apiError";
import ResourceTable, { type ResourceColumn } from "../../components/resourceTable/ResourceTable";
import { formatTime } from "../../i18n/datetime";
import SecretModal from "./SecretModal";

const { Paragraph, Text } = Typography;

/** Lifetimes offered when a token is created; 0 is "never expires". */
const EXPIRY_DAYS = [0, 30, 90, 365];

/**
 * PlatformTokensCard manages the signed-in user's platform API tokens: the
 * credential for the management API and MCP, as opposed to the gateway keys
 * that call deployed models.
 */
export default function PlatformTokensCard() {
  const { t } = useTranslation("apikeys");
  const { t: tCommon } = useTranslation("common");
  const { data, refetch, loading, error, updatedAt } = usePolling<List<PlatformToken>>(api.platformTokens, 15000);
  const [createOpen, setCreateOpen] = useState(false);
  const [secret, setSecret] = useState<string | null>(null);

  const columns: ResourceColumn<PlatformToken>[] = [
    { key: "name", role: "name", title: t("table.name"), width: 200, sortValue: (k) => k.name, render: (_: unknown, k: PlatformToken) => <Text strong>{k.name}</Text> },
    { key: "prefix", title: t("table.prefix"), width: 150, render: (_: unknown, k: PlatformToken) => <Text code>{k.prefix}</Text> },
    {
      key: "scope", title: t("platform.scope"), width: 110, sortValue: (k) => k.scope,
      render: (_: unknown, k: PlatformToken) => <Tag color={k.scope === "write" ? "orange" : "blue"}>{t(`platform.scopes.${k.scope}`)}</Tag>,
    },
    { key: "last_used_at", title: t("table.lastUsed"), width: 170, sortValue: (k) => k.last_used_at ?? "", render: (_: unknown, k: PlatformToken) => formatTime(k.last_used_at ?? undefined, t("never")) },
    { key: "expires_at", title: t("platform.expiresAt"), width: 170, sortValue: (k) => k.expires_at ?? "", render: (_: unknown, k: PlatformToken) => (k.expires_at ? formatTime(k.expires_at) : t("platform.noExpiry")) },
    { key: "created_at", title: t("table.createdAt"), width: 170, sortValue: (k) => k.created_at, render: (_: unknown, k: PlatformToken) => formatTime(k.created_at) },
    {
      key: "action",
      role: "actions",
      width: 80,
      render: (_: unknown, k: PlatformToken) => (
        <Popconfirm
          title={t("platform.revokeConfirm", { name: k.name })}
          onConfirm={async () => {
            try {
              await api.deletePlatformToken(k.id);
              message.success(t("platform.revoked"));
              refetch();
            } catch (e) {
              message.error(apiErrorText(tCommon, e));
            }
          }}
          okText={t("platform.revoke")}
          cancelText={t("actions.cancel")}
          okButtonProps={{ danger: true }}
        >
          <Tooltip title={t("platform.revoke")}>
            <Button type="text" size="small" danger icon={<DeleteOutlined />} aria-label={t("platform.revoke")} />
          </Tooltip>
        </Popconfirm>
      ),
    },
  ];

  return (
    <Card
      title={t("platform.title")}
      extra={<Button type="primary" onClick={() => setCreateOpen(true)}>{t("platform.create")}</Button>}
    >
      <Paragraph type="secondary">{t("platform.description")}</Paragraph>
      <ResourceTable<PlatformToken>
        tableId="platform-tokens"
        rowKey="id"
        columns={columns}
        data={data?.items ?? []}
        loading={loading}
        error={error}
        onRefresh={refetch}
        updatedAt={updatedAt}
        emptyKind="data"
        emptyDescription={t("platform.empty")}
      />
      <Alert
        style={{ marginTop: 16 }}
        type="info"
        message={t("usage")}
        description={
          <pre className="code-block">
            {`curl ${window.location.origin}/api/v1/nodes \\
  -H "Authorization: Bearer <gwp_token>"`}
          </pre>
        }
      />
      <CreateModal
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        onCreated={(token) => {
          setCreateOpen(false);
          setSecret(token);
          refetch();
        }}
      />
      <SecretModal secret={secret} onClose={() => setSecret(null)} />
    </Card>
  );
}

function CreateModal({ open, onClose, onCreated }: { open: boolean; onClose: () => void; onCreated: (token: string) => void }) {
  const { t } = useTranslation("apikeys");
  const { t: tCommon } = useTranslation("common");
  const [form] = Form.useForm<{ name: string; scope: PlatformToken["scope"]; expires_in_days: number }>();
  const [submitting, setSubmitting] = useState(false);
  const submittingRef = useRef(false);

  const submit = async () => {
    if (submittingRef.current) return;
    submittingRef.current = true;
    try {
      let v;
      try {
        v = await form.validateFields();
      } catch {
        // Form.Item already associates validation errors with their fields.
        return;
      }
      setSubmitting(true);
      const res = await api.createPlatformToken({ name: v.name.trim(), scope: v.scope, expires_in_days: v.expires_in_days || undefined });
      form.resetFields();
      onCreated(res.token);
    } catch (e) {
      message.error(apiErrorText(tCommon, e));
    } finally {
      submittingRef.current = false;
      setSubmitting(false);
    }
  };

  return (
    <Modal title={t("platform.createTitle")} open={open} onCancel={onClose} onOk={submit} confirmLoading={submitting} okText={t("actions.create")} cancelText={t("actions.cancel")} destroyOnClose>
      <Form form={form} layout="vertical" initialValues={{ scope: "read", expires_in_days: 0 }}>
        <Form.Item label={t("table.name")} name="name" rules={[
          { required: true, whitespace: true, message: t("create.nameRequired") },
          { validator: (_, value: string | undefined) => new TextEncoder().encode(value?.trim() ?? "").length <= 128
            ? Promise.resolve()
            : Promise.reject(new Error(t("platform.nameTooLong"))) },
        ]}>
          <Input placeholder={t("platform.namePlaceholder")} onPressEnter={() => void submit()} />
        </Form.Item>
        <Form.Item label={t("platform.scope")} name="scope" extra={t("platform.scopeHelp")}>
          <Radio.Group
            options={[
              { value: "read", label: t("platform.scopes.read") },
              { value: "write", label: t("platform.scopes.write") },
            ]}
          />
        </Form.Item>
        <Form.Item label={t("platform.expiresAt")} name="expires_in_days">
          <Select
            aria-label={t("platform.expiresAt")}
            options={EXPIRY_DAYS.map((days) => ({ value: days, label: days === 0 ? t("platform.noExpiry") : t("platform.days", { count: days }) }))}
          />
        </Form.Item>
      </Form>
    </Modal>
  );
}

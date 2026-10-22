import { useRef, useState } from "react";
import {
  Alert,
  Button,
  Card,
  Form,
  Input,
  Modal,
  Popconfirm,
  Tooltip,
  Typography,
  message,
} from "antd";
import { DeleteOutlined } from "@ant-design/icons";
import { api, usePolling, type ApiKey, type List } from "../api";
import ResourceTable, { type ResourceColumn } from "../components/resourceTable/ResourceTable";
import PageHeader from "../components/PageHeader";
import { formatTime } from "../i18n/datetime";
import { useTranslation } from "react-i18next";
import PlatformTokensCard from "./apikeys/PlatformTokensCard";
import SecretModal from "./apikeys/SecretModal";

const { Paragraph, Text } = Typography;

export default function APIKeysPage() {
  const { t } = useTranslation("apikeys");
  const { data, refetch, loading, error, updatedAt } = usePolling<List<ApiKey>>(api.apiKeys, 15000);
  const [createOpen, setCreateOpen] = useState(false);
  const [secret, setSecret] = useState<string | null>(null);

  // A token has no detail of its own, so its name is plain text rather than a link.
  const columns: ResourceColumn<ApiKey>[] = [
    { key: "name", role: "name", title: t("table.name"), width: 220, sortValue: (k) => k.name, render: (_: unknown, k: ApiKey) => <Text strong>{k.name || "—"}</Text> },
    { key: "prefix", title: t("table.prefix"), width: 180, render: (_: unknown, k: ApiKey) => <Text code>{k.prefix}</Text> },
    { key: "last_used", title: t("table.lastUsed"), width: 180, sortValue: (k) => k.last_used, render: (_: unknown, k: ApiKey) => formatTime(k.last_used, t("never")) },
    { key: "created_at", title: t("table.createdAt"), width: 180, sortValue: (k) => k.created_at, render: (_: unknown, k: ApiKey) => formatTime(k.created_at) },
    {
      key: "action",
      role: "actions",
      width: 90,
      render: (_: unknown, k: ApiKey) => (
        <Popconfirm
          title={t("delete.confirm", { name: k.name })}
          onConfirm={async () => {
            try {
              await api.deleteApiKey(k.prefix);
              message.success(t("delete.success"));
              refetch();
            } catch (e) {
              message.error(t("delete.failed", { detail: (e as Error).message }));
            }
          }}
          okText={t("actions.delete")}
          cancelText={t("actions.cancel")}
          okButtonProps={{ danger: true }}
        >
          <Tooltip title={t("actions.delete")}>
            <Button type="text" size="small" danger icon={<DeleteOutlined />} aria-label={t("actions.delete")} />
          </Tooltip>
        </Popconfirm>
      ),
    },
  ];

  // Integration examples use the address this page was opened from.
  const gateway = window.location.origin;

  return (
    <>
      <PageHeader
        title={t("title")}
        description={t("description")}
        breadcrumb={[t("breadcrumb.platform"), t("title")]}
      />
      <Card
        title={t("gateway.title")}
        style={{ marginBottom: 16 }}
        extra={<Button type="primary" onClick={() => setCreateOpen(true)}>{t("create.button")}</Button>}
      >
      <Paragraph type="secondary">{t("gateway.description")}</Paragraph>
      <ResourceTable<ApiKey>
        tableId="api-keys"
        rowKey="prefix"
        columns={columns}
        data={data?.items ?? []}
        loading={loading}
        error={error}
        onRefresh={refetch}
        updatedAt={updatedAt}
        emptyKind="data"
        emptyDescription={t("empty")}
      />

      <Alert
        style={{ marginTop: 16 }}
        type="info"
        message={t("usage")}
        description={
          <Paragraph style={{ margin: 0 }}>
            <pre className="code-block">
              {`curl ${gateway}/v1/chat/completions \\
  -H "Authorization: Bearer <key>" \\
  -d '{"model":"<served_name>","messages":[{"role":"user","content":"hi"}]}'`}
            </pre>
          </Paragraph>
        }
      />

      <CreateModal
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        onCreated={(key) => {
          setCreateOpen(false);
          setSecret(key);
          refetch();
        }}
      />
      <SecretModal secret={secret} onClose={() => setSecret(null)} />
      </Card>
      <PlatformTokensCard />
    </>
  );
}

function CreateModal({
  open,
  onClose,
  onCreated,
}: {
  open: boolean;
  onClose: () => void;
  onCreated: (key: string) => void;
}) {
  const { t } = useTranslation("apikeys");
  const [form] = Form.useForm();
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
        // Form.Item presents validation errors beside the corresponding field.
        return;
      }
      setSubmitting(true);
      const res = await api.createApiKey(v.name);
      form.resetFields();
      onCreated(res.key);
    } catch (e) {
      message.error(t("create.failed", { detail: (e as Error).message }));
    } finally {
      submittingRef.current = false;
      setSubmitting(false);
    }
  };

  return (
    <Modal
      title={t("create.title")}
      open={open}
      onCancel={onClose}
      onOk={submit}
      confirmLoading={submitting}
      okText={t("actions.create")}
      cancelText={t("actions.cancel")}
      destroyOnClose
    >
      <Form form={form} layout="vertical">
        <Form.Item label={t("table.name")} name="name" rules={[{ required: true, whitespace: true, message: t("create.nameRequired") }]}>
          <Input placeholder={t("create.namePlaceholder")} onPressEnter={() => void submit()} />
        </Form.Item>
      </Form>
    </Modal>
  );
}

import { useRef, useState } from "react";
import { Alert, Button, Collapse, Drawer, Form, Input, InputNumber, Select, Space, Switch, Typography } from "antd";
import type { InputRef } from "antd";
import { useTranslation } from "react-i18next";
import { api, type KubeCluster, type KubeClusterRequest, type KubeVendor } from "../../api";
import { APIError, apiErrorText } from "../../apiError";
import { KEEP_BORROWED_CARDS, KUBE_DEFAULTS, KUBE_VENDORS } from "../../kubeClusters";
import ApplyCommand from "./ApplyCommand";

interface FormValues {
  name: string;
  server_url: string;
  vendor: KubeVendor;
  node_selector: string;
  namespace: string;
  state_dir: string;
  agent_image: string;
  image_pull_secret: string;
  runtime_class: string;
  containerd_socket: string;
  api_server: string;
  engine_ports: string;
  keep_cards: boolean;
  idle_return_seconds: number;
}

// The request fields a refused cluster can name (KUBE_CLUSTER_INVALID's field)
// and the form field each one belongs to.
const FIELD_OF: Record<string, keyof FormValues> = {
  name: "name",
  server_url: "server_url",
  vendor: "vendor",
  state_dir: "state_dir",
  node_selector: "node_selector",
  namespace: "namespace",
  agent_image: "agent_image",
  image_pull_secret: "image_pull_secret",
  runtime_class: "runtime_class",
  containerd_socket: "containerd_socket",
  api_server: "api_server",
  engine_ports: "engine_ports",
  idle_return_seconds: "idle_return_seconds",
};

function requestOf(values: FormValues): KubeClusterRequest {
  const request: KubeClusterRequest = {
    name: values.name.trim(),
    server_url: values.server_url.trim(),
    vendor: values.vendor,
    idle_return_seconds: values.keep_cards ? KEEP_BORROWED_CARDS : values.idle_return_seconds,
  };
  // Only NVIDIA nodes get their devices through a RuntimeClass (the others through the agent's device plugin). Every
  // empty field takes the server's default; an empty RuntimeClass is decided on each node.
  const optional = { runtime_class: values.vendor === "nvidia" ? values.runtime_class : "", image_pull_secret: values.image_pull_secret, namespace: values.namespace, state_dir: values.state_dir, agent_image: values.agent_image, node_selector: values.node_selector, containerd_socket: values.containerd_socket, api_server: values.api_server, engine_ports: values.engine_ports } as const;
  for (const [key, value] of Object.entries(optional)) {
    const text = (value ?? "").trim();
    if (text) (request as unknown as Record<string, string>)[key] = text;
  }
  return request;
}

/**
 * Connects a Kubernetes cluster: the form, then the command to run in the
 * cluster. The list reloads as soon as the cluster exists; the drawer stays
 * on the command until it is closed.
 */
export default function KubeClusterDrawer({ open, onClose, onCreated }: { open: boolean; onClose: () => void; onCreated: () => void }) {
  const { t } = useTranslation("kubernetes");
  const { t: tCommon } = useTranslation("common");
  const [form] = Form.useForm<FormValues>();
  const keepCards = Form.useWatch("keep_cards", form);
  const vendor = Form.useWatch("vendor", form);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [created, setCreated] = useState<KubeCluster | null>(null);
  const nameInput = useRef<InputRef>(null);

  const submit = async (values: FormValues) => {
    setSaving(true);
    setError(null);
    try {
      const cluster = await api.createKubeCluster(requestOf(values));
      setCreated(cluster);
      onCreated();
    } catch (e) {
      const text = apiErrorText(tCommon, e);
      const field = e instanceof APIError
        ? e.code === "KUBE_CLUSTER_NAME_TAKEN" ? "name" : e.code === "KUBE_CLUSTER_INVALID" ? FIELD_OF[String(e.params?.field ?? "")] : undefined
        : undefined;
      if (field) form.setFields([{ name: field, errors: [text] }]);
      else setError(text);
    } finally {
      setSaving(false);
    }
  };

  const advanced = (
    <>
      <Form.Item name="namespace" label={t("drawer.namespace")} extra={t("drawer.namespaceHelp")}>
        <Input />
      </Form.Item>
      <Form.Item name="agent_image" label={t("drawer.agentImage")} extra={t("drawer.agentImageHelp")}>
        <Input placeholder={t("drawer.agentImagePlaceholder")} />
      </Form.Item>
      <Form.Item name="image_pull_secret" label={t("drawer.imagePullSecret")} extra={t("drawer.imagePullSecretHelp")}>
        <Input placeholder={t("drawer.imagePullSecretPlaceholder")} />
      </Form.Item>
      <Form.Item name="state_dir" label={t("drawer.stateDir")} extra={t("drawer.stateDirHelp")}>
        <Input placeholder={KUBE_DEFAULTS.stateDir} />
      </Form.Item>
      {vendor === "nvidia" && (
        <Form.Item name="runtime_class" label={t("drawer.runtimeClass")} extra={t("drawer.runtimeClassHelp")}>
          <Input allowClear placeholder={t("drawer.runtimeClassPlaceholder")} />
        </Form.Item>
      )}
      <Form.Item name="containerd_socket" label={t("drawer.containerdSocket")} extra={t("drawer.containerdSocketHelp")}>
        <Input placeholder={t("drawer.containerdSocketPlaceholder")} />
      </Form.Item>
      <Form.Item name="api_server" label={t("drawer.apiServer")} extra={t("drawer.apiServerHelp")}>
        <Input placeholder={t("drawer.apiServerPlaceholder")} />
      </Form.Item>
      <Form.Item name="engine_ports" label={t("drawer.enginePorts")} extra={t("drawer.enginePortsHelp")}>
        <Input />
      </Form.Item>
      <Form.Item label={t("drawer.idleReturn")} extra={keepCards ? t("drawer.keepCardsHelp") : t("drawer.idleReturnHelp")}>
        <Space align="center" wrap>
          <Form.Item name="keep_cards" valuePropName="checked" noStyle>
            <Switch aria-label={t("drawer.keepCards")} />
          </Form.Item>
          <Typography.Text>{t("drawer.keepCards")}</Typography.Text>
          <Form.Item name="idle_return_seconds" noStyle>
            <InputNumber min={0} precision={0} disabled={!!keepCards} aria-label={t("drawer.idleReturnSeconds")} addonAfter="s" />
          </Form.Item>
        </Space>
      </Form.Item>
    </>
  );

  return (
    <Drawer
      title={t("drawer.title")}
      open={open}
      onClose={onClose}
      width={560}
      destroyOnClose
      // The drawer takes focus itself when it opens; hand it to the first field once it has.
      afterOpenChange={(visible) => {
        if (visible) nameInput.current?.focus();
        else { setCreated(null); setError(null); }
      }}
      footer={created ? (
        <Button type="primary" onClick={onClose}>{t("drawer.close")}</Button>
      ) : (
        <Space>
          <Button type="primary" htmlType="submit" form="kube-cluster-form" loading={saving}>{t("drawer.submit")}</Button>
          <Button onClick={onClose}>{t("drawer.cancel")}</Button>
        </Space>
      )}
    >
      {created ? (
        <Space direction="vertical" size={16} style={{ width: "100%" }}>
          <Alert type="success" showIcon message={t("created.title", { name: created.name })} description={t("created.note")} />
          {created.apply_command && <ApplyCommand command={created.apply_command} />}
          <Typography.Text type="secondary">{t("created.secret")}</Typography.Text>
        </Space>
      ) : (
        <Form<FormValues>
          id="kube-cluster-form"
          form={form}
          layout="vertical"
          requiredMark
          onFinish={(values) => void submit(values)}
          initialValues={{
            name: "",
            server_url: window.location.origin,
            vendor: KUBE_DEFAULTS.vendor,
            node_selector: "",
            namespace: KUBE_DEFAULTS.namespace,
            state_dir: "",
            agent_image: "",
            image_pull_secret: "",
            runtime_class: "",
            containerd_socket: "",
            api_server: "",
            engine_ports: KUBE_DEFAULTS.enginePorts,
            keep_cards: false,
            idle_return_seconds: KUBE_DEFAULTS.idleReturnSeconds,
          }}
        >
          {/* The footer's button is outside the form; this one lets Enter in a field submit it. */}
          <button type="submit" hidden aria-hidden="true" tabIndex={-1} />
          {error && <Alert type="error" showIcon style={{ marginBottom: 16 }} message={error} />}
          <Form.Item name="name" label={t("drawer.name")} rules={[{ required: true, whitespace: true, message: t("drawer.nameRequired") }]}>
            <Input ref={nameInput} placeholder={t("drawer.namePlaceholder")} />
          </Form.Item>
          <Form.Item
            name="server_url"
            label={t("drawer.serverUrl")}
            extra={t("drawer.serverUrlHelp")}
            rules={[
              { required: true, whitespace: true, message: t("drawer.serverUrlRequired") },
              { pattern: /^\s*https?:\/\/\S+\s*$/i, message: t("drawer.serverUrlInvalid") },
            ]}
          >
            <Input />
          </Form.Item>
          <Form.Item name="vendor" label={t("drawer.vendor")} extra={t("drawer.vendorHelp")}>
            <Select options={KUBE_VENDORS.map((value) => ({ value, label: tCommon(`vendor.${value}`) }))} />
          </Form.Item>
          <Form.Item name="node_selector" label={t("drawer.nodeSelector")} extra={t("drawer.nodeSelectorHelp")}>
            <Input placeholder={t("drawer.nodeSelectorPlaceholder")} />
          </Form.Item>
          <Collapse ghost items={[{ key: "advanced", label: t("drawer.advanced"), forceRender: true, children: advanced }]} />
        </Form>
      )}
    </Drawer>
  );
}

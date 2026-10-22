import { useEffect, useState, type ReactNode } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import {
  Alert,
  Button,
  Card,
  Form,
  Input,
  InputNumber,
  Modal,
  Popconfirm,
  Select,
  Space,
  Switch,
  Table,
  Tabs,
  Tag,
  Typography,
  message,
} from "antd";
import type { ColumnsType } from "antd/es/table";
import PageHeader from "../components/PageHeader";
import {
  api,
  type AIConfig,
  type AIPreset,
} from "../api";
import { defaultTerminalSettings, type TerminalSettings } from "../terminal";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import ServerListenCard from "./settings/ServerListenCard";
import PlatformSSHKeyCard from "./settings/PlatformSSHKeyCard";
import DeploymentPolicyCard from "./settings/DeploymentPolicyCard";
import SecurityCard from "./settings/SecurityCard";
import UpdatesCard from "./settings/UpdatesCard";
import MCPCard from "./settings/MCPCard";
import TelemetryCard from "./settings/TelemetryCard";
import PullSettingsCard from "./settings/pull/PullSettingsCard";
import BrandingCard, { BRANDING_ANCHOR } from "./settings/BrandingCard";
import { TELEMETRY_ANCHOR } from "./settings/TelemetryCard";
import { SECURITY_ANCHOR } from "./settings/SecurityCard";

const { Text } = Typography;

/**
 * SETTINGS_TABS groups the settings cards. A tab is addressed by its key as
 * the URL fragment (/settings#access); a card's own fragment
 * (/settings#security) opens the tab holding it, so links into the page keep
 * working.
 */
const SETTINGS_TABS: { key: string; anchors: string[]; cards: () => ReactNode }[] = [
  {
    key: "general",
    anchors: [BRANDING_ANCHOR, "updates"],
    cards: () => <><BrandingCard /><UpdatesCard /><ServerListenCard /></>,
  },
  { key: "pull", anchors: ["pull-settings"], cards: () => <PullSettingsCard /> },
  {
    key: "nodes",
    anchors: [],
    cards: () => <><PlatformSSHKeyCard /><DeploymentPolicyCard /><OrphanCleanupCard /></>,
  },
  {
    key: "access",
    anchors: [SECURITY_ANCHOR],
    cards: () => <><SecurityCard /><TerminalCard /><MCPCard /></>,
  },
  { key: "integrations", anchors: [TELEMETRY_ANCHOR], cards: () => <><AICard /><TelemetryCard /></> },
];

/** settingsTabFor returns the tab a URL fragment selects (the first by default). */
export function settingsTabFor(hash: string): string {
  const fragment = hash.replace(/^#/, "");
  return SETTINGS_TABS.find((tab) => tab.key === fragment || tab.anchors.includes(fragment))?.key ?? SETTINGS_TABS[0].key;
}

export default function SettingsPage() {
  const { t } = useTranslation("settings");
  const { hash } = useLocation();
  const navigate = useNavigate();
  const active = settingsTabFor(hash);
  // A card's fragment opens its tab; then bring the card into view once the
  // tab has rendered it (the browser's own jump ran against an empty page).
  useEffect(() => {
    const fragment = hash.replace(/^#/, "");
    if (!fragment || SETTINGS_TABS.some((tab) => tab.key === fragment)) return;
    let frame = 0;
    const deadline = Date.now() + 2000;
    const jump = () => {
      const card = document.getElementById(fragment);
      if (card) card.scrollIntoView?.({ block: "start" });
      else if (Date.now() < deadline) frame = requestAnimationFrame(jump);
    };
    jump();
    return () => cancelAnimationFrame(frame);
  }, [hash]);
  return (
    <>
      <PageHeader title={t("page.title")} description={t("page.description")} breadcrumb={[t("page.breadcrumb.platform"), t("page.breadcrumb.settings")]} />
      <Tabs
        className="settings-tabs"
        activeKey={active}
        onChange={(key) => navigate({ hash: key }, { replace: true })}
        items={SETTINGS_TABS.map((tab) => ({
          key: tab.key,
          label: t(`page.tabs.${tab.key}`),
          children: <Space direction="vertical" size={16} style={{ width: "100%" }}>{tab.cards()}</Space>,
        }))}
      />
    </>
  );
}

function TerminalCard() {
  const { t } = useTranslation("settings");
  const [cfg, setCfg] = useState<TerminalSettings>(defaultTerminalSettings);
  const [saving, setSaving] = useState(false);
  useEffect(() => {
    api.settings()
      .then((settings) => {
        const value = settings?.terminal as Partial<TerminalSettings> | undefined;
        if (value) setCfg((current) => ({ ...current, ...value }));
      })
      .catch(() => {});
  }, []);
  const save = async () => {
    setSaving(true);
    try {
      await api.putSetting("terminal", cfg);
      message.success(t("terminal.saved"));
    } catch (e) {
      message.error(t("error.save", { detail: (e as Error).message }));
    } finally {
      setSaving(false);
    }
  };
  const field = (label: string, hint: string, value: number, min: number, max: number, set: (v: number) => void) => (
    <Form.Item label={label} extra={hint} style={{ marginBottom: 12 }}>
      <InputNumber min={min} max={max} value={value} onChange={(v) => set(Number(v ?? min))} style={{ width: 160 }} />
    </Form.Item>
  );
  return (
    <Card title={t("terminal.title")} extra={<Button type="primary" loading={saving} onClick={save}>{t("common.save")}</Button>}>
      <Space direction="vertical" size={4} style={{ width: "100%", maxWidth: 720 }}>
        <Text type="secondary">{t("terminal.description")} <Text strong>{t("terminal.privacy")}</Text></Text>
        <Form layout="vertical" style={{ marginTop: 8 }}>
          <Form.Item label={t("terminal.enabled")} extra={t("terminal.enabledHelp")} style={{ marginBottom: 12 }}>
            <Switch checked={cfg.enabled} onChange={(enabled) => setCfg((c) => ({ ...c, enabled }))} />
          </Form.Item>
          {field(t("terminal.idle"), t("terminal.idleHelp"), cfg.idle_timeout_min, 1, 1440, (v) => setCfg((c) => ({ ...c, idle_timeout_min: v })))}
          {field(t("terminal.grace"), t("terminal.graceHelp"), cfg.grace_sec, 1, 120, (v) => setCfg((c) => ({ ...c, grace_sec: v })))}
          {field(t("terminal.nodeLimit"), t("terminal.nodeLimitHelp"), cfg.max_per_node, 1, 64, (v) => setCfg((c) => ({ ...c, max_per_node: v })))}
          {field(t("terminal.globalLimit"), t("terminal.globalLimitHelp"), cfg.max_global, 1, 512, (v) => setCfg((c) => ({ ...c, max_global: v })))}
        </Form>
      </Space>
    </Card>
  );
}

function OrphanCleanupCard() {
  const { t } = useTranslation("settings");
  const [enabled, setEnabled] = useState(false);
  const [saving, setSaving] = useState(false);
  useEffect(() => {
    api.settings()
      .then((settings) => {
        setEnabled(settings?.orphan_cleanup === true);
      })
      .catch(() => {});
  }, []);
  const save = async () => {
    setSaving(true);
    try {
      await api.putSetting("orphan_cleanup", enabled);
      message.success(t("common.saved"));
    } catch (e) {
      message.error(t("error.save", { detail: (e as Error).message }));
    } finally {
      setSaving(false);
    }
  };
  return (
    <Card title={t("orphanCleanup.title")} extra={<Button type="primary" loading={saving} onClick={save}>{t("common.save")}</Button>}>
      <Space direction="vertical" size={4} style={{ width: "100%", maxWidth: 720 }}>
        <Text type="secondary">{t("orphanCleanup.description")}</Text>
        <Form layout="vertical" style={{ marginTop: 8 }}>
          <Form.Item label={t("orphanCleanup.enabled")} style={{ marginBottom: 0 }}>
            <Switch checked={enabled} onChange={setEnabled} />
          </Form.Item>
        </Form>
      </Space>
    </Card>
  );
}

const PRESETS: { value: AIPreset; base_url: string; model: string }[] = [
  { value: "deepseek", base_url: "https://api.deepseek.com", model: "deepseek-v4-flash" },
  {
    value: "bailian",
    base_url: "https://dashscope.aliyuncs.com/compatible-mode/v1",
    model: "qwen-plus",
  },
  { value: "siliconflow", base_url: "https://api.siliconflow.cn/v1", model: "" },
  { value: "openrouter", base_url: "https://openrouter.ai/api/v1", model: "" },
  { value: "poe", base_url: "https://api.poe.com/v1", model: "GPT-5" },
  { value: "custom", base_url: "", model: "" },
];

const presetLabel = (t: TFunction, preset: string) => PRESETS.some((item) => item.value === preset) ? t(`ai.presetName.${preset}`) : preset;

/**
 * externalHost is the host an AI endpoint sends data to, or "" when the
 * endpoint is this platform itself (a model served here, reached through its
 * own gateway) - then nothing leaves the platform.
 */
export function externalHost(baseURL: string, origin: string = window.location.origin): string {
  try {
    const url = new URL(baseURL, origin);
    return url.origin === new URL(origin).origin ? "" : url.host;
  } catch {
    return baseURL;
  }
}

function AICard() {
  const { t } = useTranslation("settings");
  const [configs, setConfigs] = useState<AIConfig[]>([]);
  const [addOpen, setAddOpen] = useState(false);

  const load = () => api.aiConfigs().then((list) => setConfigs(list.items)).catch(() => {});
  // The assistant uses the default endpoint; say where its reads go when that
  // is outside the platform.
  const active = configs.find((c) => c.is_default) ?? configs[0];
  const egressHost = active ? externalHost(active.base_url) : "";
  useEffect(() => {
    load();
  }, []);

  const del = async (id: number) => {
    try {
      await api.deleteAIConfig(id);
      message.success(t("ai.deleted"));
      load();
    } catch (e) {
      message.error(t("error.delete", { detail: (e as Error).message }));
    }
  };

  const columns: ColumnsType<AIConfig> = [
    { title: t("ai.name"), dataIndex: "name" },
    { title: t("ai.preset"), dataIndex: "preset", width: 180, render: (preset: string) => presetLabel(t, preset) },
    { title: t("ai.model"), dataIndex: "model", render: (model: string) => model || "—" },
    {
      title: t("ai.default"),
      dataIndex: "is_default",
      width: 80,
      render: (isDefault: boolean) => (isDefault ? <Tag color="green">{t("ai.default")}</Tag> : "—"),
    },
    {
      title: "",
      key: "action",
      width: 80,
      render: (_: unknown, c: AIConfig) => (
        <Popconfirm title={t("ai.deleteConfirm")} onConfirm={() => del(c.id)} okText={t("common.delete")} cancelText={t("common.cancel")}>
          <Button type="link" danger size="small">
            {t("common.delete")}
          </Button>
        </Popconfirm>
      ),
    },
  ];

  return (
    <Card
      title={t("ai.service")}
      extra={
        <Button type="primary" onClick={() => setAddOpen(true)}>
          {t("common.add")}
        </Button>
      }
    >
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message={t("ai.notice")}
      />
      {egressHost && <Alert type="warning" showIcon style={{ marginBottom: 16 }} message={t("ai.egressNotice", { host: egressHost })} />}
      <Table<AIConfig>
        rowKey="id"
        columns={columns}
        dataSource={configs}
        pagination={false}
        locale={{ emptyText: t("ai.empty") }}
      />
      <AddAIModal open={addOpen} onClose={() => setAddOpen(false)} onDone={() => { setAddOpen(false); load(); }} />
    </Card>
  );
}

function AddAIModal({
  open,
  onClose,
  onDone,
}: {
  open: boolean;
  onClose: () => void;
  onDone: () => void;
}) {
  const { t } = useTranslation("settings");
  const [form] = Form.useForm();
  const [submitting, setSubmitting] = useState(false);

  const onPreset = (value: AIPreset) => {
    const p = PRESETS.find((x) => x.value === value);
    if (p) form.setFieldsValue({ base_url: p.base_url, model: p.model });
  };

  const submit = async () => {
    const v = await form.validateFields();
    setSubmitting(true);
    try {
      await api.createAIConfig({
        name: v.name,
        preset: v.preset,
        base_url: v.base_url,
        api_key: v.api_key,
        model: v.model,
        is_default: !!v.is_default,
      });
      message.success(t("ai.added"));
      form.resetFields();
      onDone();
    } catch (e) {
      message.error(t("error.add", { detail: (e as Error).message }));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal
      title={t("ai.addService")}
      open={open}
      onCancel={onClose}
      onOk={submit}
      confirmLoading={submitting}
      okText={t("common.add")}
      cancelText={t("common.cancel")}
      destroyOnClose
    >
      <Form
        form={form}
        layout="vertical"
        initialValues={{ preset: "deepseek", base_url: "https://api.deepseek.com", model: "deepseek-v4-flash", is_default: true }}
      >
        <Form.Item label={t("ai.preset")} name="preset" rules={[{ required: true }]}>
          <Select options={PRESETS.map((preset) => ({ value: preset.value, label: presetLabel(t, preset.value) }))} onChange={onPreset} />
        </Form.Item>
        <Form.Item label={t("ai.name")} name="name" rules={[{ required: true, message: t("ai.nameRequired") }]}>
          <Input placeholder={t("ai.namePlaceholder")} />
        </Form.Item>
        <Form.Item label={t("ai.baseUrl")} name="base_url" rules={[{ required: true, message: t("ai.baseUrlRequired") }]}>
          <Input placeholder="https://api.deepseek.com" />
        </Form.Item>
        <Form.Item label={t("ai.model")} name="model" rules={[{ required: true, message: t("ai.modelRequired") }]}>
          <Input placeholder="deepseek-v4-flash" />
        </Form.Item>
        <Form.Item label={t("ai.upstreamKey")} name="api_key" rules={[{ required: true, message: t("ai.keyRequired") }]}>
          <Input.Password placeholder="sk-…" autoComplete="new-password" />
        </Form.Item>
        <Form.Item label={t("ai.setDefault")} name="is_default" valuePropName="checked">
          <Switch />
        </Form.Item>
      </Form>
    </Modal>
  );
}

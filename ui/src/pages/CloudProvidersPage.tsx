import { useEffect, useState } from "react";
import {
  Alert,
  Button,
  Popconfirm,
  Space,
  Tag,
  Tooltip,
  Typography,
  message,
} from "antd";
import { DeleteOutlined, EditOutlined, SyncOutlined } from "@ant-design/icons";
import {
  api,
  usePolling,
  type CloudProvider,
  type List,
  type ProviderKindInfo,
} from "../api";
import PageHeader from "../components/PageHeader";
import ResourceTable, { type ResourceColumn } from "../components/resourceTable/ResourceTable";
import ProviderConfigModal from "../components/ProviderConfigModal";
import ProviderInstancesTable from "../components/ProviderInstancesTable";
import { formatTime } from "../i18n/datetime";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { LocalizedText } from "../i18n/localizedText";
import { localizedText } from "../i18n/localizedText";

type ProviderScope = "public" | "private";

function belongsToScope(kind: string, scope: ProviderScope): boolean {
  return scope === "private" ? kind === "zstack" : kind !== "zstack";
}

export default function CloudProvidersPage({ scope = "public" }: { scope?: ProviderScope }) {
  const { t, i18n } = useTranslation("providers");
  const language = i18n.resolvedLanguage || i18n.language;
  const copy = {
    title: t(`scope.${scope}.title`),
    description: t(`scope.${scope}.description`),
    empty: t(`scope.${scope}.empty`),
  };
  const { data: allProviders, refetch, loading, error, updatedAt } = usePolling<List<CloudProvider>>(api.providers, 15000);
  const data = (allProviders?.items ?? []).filter((provider) => belongsToScope(provider.kind, scope));
  const [modalOpen, setModalOpen] = useState(false);
  const [defaultKind, setDefaultKind] = useState<string | null>(null);
  const [editingProvider, setEditingProvider] = useState<CloudProvider | null>(null);
  const [syncing, setSyncing] = useState<number | null>(null);
  const [syncingAll, setSyncingAll] = useState(false);
  const [instanceRefreshVersion, setInstanceRefreshVersion] = useState(0);
  const [kindInfos, setKindInfos] = useState<ProviderKindInfo[]>([]);

  useEffect(() => {
    api
      .providerKinds()
      .then((infos) => setKindInfos(infos.items.filter((kind) => belongsToScope(kind.kind, scope))))
      .catch(() => setKindInfos([]));
  }, [scope]);

  const openAdd = (kind?: string) => {
    setEditingProvider(null);
    setDefaultKind(kind ?? null);
    setModalOpen(true);
  };

  const openEdit = (provider: CloudProvider) => {
    setDefaultKind(null);
    setEditingProvider(provider);
    setModalOpen(true);
  };

  const closeModal = () => {
    setModalOpen(false);
    setDefaultKind(null);
    setEditingProvider(null);
  };

  const doSync = async (id: number) => {
    setSyncing(id);
    try {
      const res = await api.syncProvider(id);
      message.success(t("message.syncDone", { count: res.count }));
      await refetch();
      setInstanceRefreshVersion((version) => version + 1);
    } catch (e) {
      message.error(t("message.syncFailed", { detail: (e as Error).message }));
    } finally {
      setSyncing(null);
    }
  };

  const doSyncAll = async () => {
    const providers = data.filter((provider) => {
      const kindInfo = kindInfos.find((kind) => kind.kind === provider.kind);
      return kindInfo?.capabilities.can_list_instances !== false;
    });
    if (providers.length === 0) return;
    setSyncingAll(true);
    try {
      let ok = 0;
      let failed = 0;
      for (const provider of providers) {
        try {
          await api.syncProvider(provider.id);
          ok += 1;
        } catch {
          failed += 1;
        }
      }
      await refetch();
      setInstanceRefreshVersion((version) => version + 1);
      if (failed === 0) {
        message.success(t("message.syncAllDone", { count: ok, scope: copy.title }));
      } else {
        message.warning(t("message.syncAllPartial", { ok, failed }));
      }
    } finally {
      setSyncingAll(false);
    }
  };

  const summary = {
    total: data.length,
    invalid: data.filter((p) => p.state === "invalid").length,
    instances: data.reduce((sum, p) => sum + (p.instance_count ?? 0), 0),
  };
  const syncableCount = data.filter((provider) => {
    const kindInfo = kindInfos.find((kind) => kind.kind === provider.kind);
    return kindInfo?.capabilities.can_list_instances !== false;
  }).length;

  const kindsPresent = [...new Set(data.map((provider) => provider.kind))];
  const columns: ResourceColumn<CloudProvider>[] = [
    {
      key: "name",
      role: "name",
      title: t("table.column.name"),
      width: 220,
      sortValue: (p) => p.name,
      render: (_: unknown, p: CloudProvider) => <Button type="link" onClick={() => openEdit(p)}>{p.name || "—"}</Button>,
    },
    {
      key: "kind",
      title: t("table.column.type"),
      width: 160,
      sortValue: (p) => p.kind,
      filters: kindsPresent.map((kind) => ({ value: kind, text: providerKindLabel(t, kind, kindInfos.find((info) => info.kind === kind)?.label, language) })),
      onFilter: (value, p) => p.kind === value,
      render: (_: unknown, p: CloudProvider) => <Tag color="blue">{providerKindLabel(t, p.kind, undefined, language)}</Tag>,
    },
    { key: "instance_count", title: t("table.column.instances"), width: 100, align: "right", sortValue: (p) => p.instance_count ?? 0, render: (_: unknown, p: CloudProvider) => `${p.instance_count ?? 0}` },
    { key: "updated_at", title: t("table.column.updatedAt"), width: 180, sortValue: (p) => p.updated_at, render: (_: unknown, p: CloudProvider) => formatTime(p.updated_at) },
    {
      key: "state",
      title: t("table.column.status"),
      width: 110,
      sortValue: (p) => p.state,
      filters: [{ value: "valid", text: t("table.state.valid") }, { value: "invalid", text: t("table.state.invalid") }],
      onFilter: (value, p) => p.state === value,
      render: (_: unknown, p: CloudProvider) => {
        if (p.state === "valid") return <Tag color="green">{t("table.state.valid")}</Tag>;
        if (p.state === "invalid") return <Tag color="red">{t("table.state.invalid")}</Tag>;
        return <Tag>{p.state || "—"}</Tag>;
      },
    },
    {
      key: "action",
      role: "actions",
      width: 120,
      render: (_: unknown, p: CloudProvider) => {
        const kindInfo = kindInfos.find((k) => k.kind === p.kind);
        const syncable = kindInfo?.capabilities.can_list_instances !== false;
        return (
          <Space size={2}>
            <Tooltip title={t("table.edit")}>
              <Button type="text" size="small" icon={<EditOutlined />} aria-label={t("table.edit")} onClick={() => openEdit(p)} />
            </Tooltip>
            <Tooltip title={syncable ? t("table.sync") : t("table.comingSoon")}>
              <Button type="text" size="small" icon={<SyncOutlined />} aria-label={t("table.sync")} disabled={!syncable} loading={syncing === p.id} onClick={() => doSync(p.id)} />
            </Tooltip>
            <Popconfirm
              title={t("table.deleteConfirm", { scope: copy.title })}
              okText={t("table.delete")}
              cancelText={t("table.cancel")}
              okButtonProps={{ danger: true }}
              onConfirm={async () => {
                try {
                  await api.deleteProvider(p.id);
                  message.success(t("message.deleted"));
                  refetch();
                } catch (e) {
                  message.error(t("message.deleteFailed", { detail: (e as Error).message }));
                }
              }}
            >
              <Tooltip title={t("table.delete")}>
                <Button type="text" size="small" danger icon={<DeleteOutlined />} aria-label={t("table.delete")} />
              </Tooltip>
            </Popconfirm>
          </Space>
        );
      },
    },
  ];

  return (
    <>
      <PageHeader
        title={copy.title}
        description={copy.description}
        breadcrumb={[t("page.resourceBreadcrumb"), copy.title]}
        extra={
          <>
            <Typography.Text type="secondary">{t("page.count", { count: data.length, scope: copy.title })}</Typography.Text>
            <Button type="primary" onClick={() => openAdd()}>
              {t("page.add", { scope: copy.title })}
            </Button>
          </>
        }
      />

      <section className="operational-panel">
        <Typography.Title level={5} style={{ margin: "0 0 14px" }}>
          {t("page.supportedTypes")}
        </Typography.Title>
        {error && (
          <Alert
            type="warning"
            showIcon
            style={{ marginBottom: 12 }}
            message={t("page.listFailed", { scope: copy.title })}
            description={error.message}
          />
        )}
        <div className="provider-kind-grid">
          {kindInfos.map((kind) => {
            const label = providerKindLabel(t, kind.kind, kind.label, language);
            return (
              <div key={kind.kind} className="provider-kind-card">
                <button
                  type="button"
                  onClick={() => openAdd(kind.kind)}
                  className="provider-kind-add"
                  aria-label={t("page.addKind", { kind: label })}
                >
                  <ProviderKindLogo kind={kind.kind} />
                  <Typography.Text strong>{label}</Typography.Text>
                </button>
                {kind.homepage && (
                  <Typography.Link href={kind.homepage} target="_blank" rel="noopener noreferrer" className="provider-kind-homepage">
                    {kind.homepage.replace(/^https?:\/\//, "")}
                  </Typography.Link>
                )}
              </div>
            );
          })}
          {kindInfos.length === 0 && !error && (
            <Alert
              type="info"
              showIcon
              message={t("page.schemaMissing")}
              description={t("page.schemaMissingDescription")}
            />
          )}
        </div>
      </section>

      <section className="operational-panel">
        <div className="provider-connected-heading">
          <Typography.Title level={5} style={{ margin: 0 }}>
            {t("page.connected", { scope: copy.title })}
          </Typography.Title>
          {summary.total > 0 && (
            <Typography.Text type="secondary">
              {t("page.summary", { count: summary.total, instances: summary.instances })}
              {summary.invalid > 0 && (
                <>
                  {" · "}
                  <Typography.Text type="danger">{t("page.summaryInvalid", { count: summary.invalid })}</Typography.Text>
                </>
              )}
            </Typography.Text>
          )}
        </div>
        {summary.invalid > 0 && (
          <Alert
            type="warning"
            showIcon
            style={{ marginBottom: 12 }}
            message={t("page.invalidMessage", { scope: copy.title })}
            description={t("page.invalidDescription")}
          />
        )}
        <ResourceTable<CloudProvider>
          tableId={`providers-${scope}`}
          rowKey="id"
          error={error}
          loading={loading}
          columns={columns}
          data={data}
          onRefresh={refetch}
          updatedAt={updatedAt}
          emptyDescription={copy.empty}
          emptyKind="data"
          toolbarExtra={
            <Button onClick={doSyncAll} loading={syncingAll} disabled={syncableCount === 0}>
              {t("page.syncAll")}
            </Button>
          }
          expandable={{
            expandedRowRender: (p) => (
              <ProviderInstancesTable
                providerId={p.id}
                kindInfo={kindInfos.find((k) => k.kind === p.kind)}
                refreshKey={`${p.updated_at}:${instanceRefreshVersion}`}
                showRegion={scope === "public"}
              />
            ),
          }}
        />
      </section>

      <ProviderConfigModal
        open={modalOpen}
        provider={editingProvider}
        defaultKind={defaultKind}
        kindInfos={kindInfos}
        providerLabel={copy.title}
        onClose={closeModal}
        onSaved={() => {
          closeModal();
          refetch();
        }}
      />
    </>
  );
}

const PROVIDER_LOGOS: Record<string, string> = {
  aliyun: "/provider-logos/aliyun.webp",
  autodl: "/provider-logos/autodl.webp",
  compshare: "/provider-logos/compshare.webp",
  runpod: "/provider-logos/runpod.webp",
  vast: "/provider-logos/vast.webp",
  zstack: "/provider-logos/zstack.svg",
};

// Vendor marks are UI assets under public/provider-logos; a kind without one
// shows its initial instead.
function ProviderKindLogo({ kind }: { kind: string }) {
  const logo = PROVIDER_LOGOS[kind];
  if (logo) {
    return <img className="provider-kind-logo" src={logo} alt="" width={32} height={32} />;
  }
  return <span className="provider-kind-logo provider-kind-logo-fallback" aria-hidden="true">{kind.slice(0, 1).toUpperCase()}</span>;
}

function providerKindLabel(t: TFunction, kind: string, fallback: LocalizedText | undefined, language: string): string {
  return t(`kind.${kind}`, { defaultValue: localizedText(fallback, language) || kind });
}

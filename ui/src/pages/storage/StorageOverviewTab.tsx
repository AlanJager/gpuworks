import { Button, Descriptions, Space, Tag, Typography } from "antd";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { storageTypeInfo, type Storage, type StorageTarget } from "../../storageTypes";
import { storageTypeLabel } from "../../storageDisplay";
import { formatBytes } from "../../format";
import DetailSection from "../../components/detail/DetailSection";
import StorageCapacity from "./StorageCapacity";
import StorageControlPlanes from "./StorageControlPlanes";
import StorageTags from "./StorageTags";
import { nodeRowName } from "../../nodeDisplay";

function Path({ value, fallback }: { value?: string; fallback: string }) {
  return value ? <Typography.Text code>{value}</Typography.Text> : <Typography.Text type="secondary">{fallback}</Typography.Text>;
}

/** One entry of the applies-to list: a label, or a node with the settings that differ on it. */
function TargetLine({ target }: { target: StorageTarget }) {
  const { t } = useTranslation("storage");
  if (target.label_key) {
    return <li><Tag>{t("overview.targetLabel")}</Tag><Typography.Text code>{`${target.label_key}=${target.label_value ?? ""}`}</Typography.Text></li>;
  }
  const overrides = [
    target.path && t("overview.overridePath", { path: target.path }),
    target.cache_path && t("overview.overrideCachePath", { path: target.cache_path }),
    target.cache_max_bytes != null ? t("overview.overrideCacheMax", { size: target.cache_max_bytes === 0 ? t("overview.cacheUnlimited") : formatBytes(target.cache_max_bytes) }) : "",
    target.existing && t("overview.overrideExisting"),
  ].filter(Boolean);
  return (
    <li>
      <Tag>{t("overview.targetNode")}</Tag>{nodeRowName(target)}
      {overrides.length > 0 && <Typography.Text type="secondary"> · {overrides.join(" · ")}</Typography.Text>}
    </li>
  );
}

/** Capacity of a local storage: it is each node's own disk, so only the spread is summarised here. */
function LocalCapacity({ storage, onShowNodes }: { storage: Storage; onShowNodes: () => void }) {
  const { t } = useTranslation("storage");
  const reporting = (storage.nodes.all ?? []).filter((n) => n.total_bytes);
  const least = reporting.reduce<typeof reporting[number] | null>((m, n) => (!m || (n.available_bytes ?? 0) < (m.available_bytes ?? 0) ? n : m), null);
  const most = reporting.reduce<typeof reporting[number] | null>((m, n) => (!m || (n.available_bytes ?? 0) > (m.available_bytes ?? 0) ? n : m), null);
  return (
    <Space direction="vertical" size={2}>
      <Typography.Text type="secondary">{t("capacity.perNode")}</Typography.Text>
      {least && most
        ? <Typography.Text>{t("overview.freeSpread", { min: formatBytes(least.available_bytes ?? 0), minNode: nodeRowName(least), max: formatBytes(most.available_bytes ?? 0), maxNode: nodeRowName(most) })}</Typography.Text>
        : <Typography.Text type="secondary">{t("capacity.unknown")}</Typography.Text>}
      <Button type="link" size="small" onClick={onShowNodes} style={{ padding: 0 }}>{t("overview.showNodes")}</Button>
    </Space>
  );
}

/** Configuration, capacity, who else uses it and the deployments that use it. */
export default function StorageOverviewTab({ storage, onShowNodes }: { storage: Storage; onShowNodes: () => void }) {
  const { t } = useTranslation("storage");
  const shared = storageTypeInfo(storage.type).shared;
  const nfs = storageTypeInfo(storage.type).nfs;
  return (
    <>
      <DetailSection title={t("overview.config")}>
        <Descriptions size="small" column={1} bordered>
          <Descriptions.Item label={t("overview.type")}>{storageTypeLabel(t, storage.type)}</Descriptions.Item>
          <Descriptions.Item label={t("overview.state")}>
            <Space size={4} wrap>
              <Typography.Text>{t(storage.enabled ? "overview.enabled" : "overview.disabled")}</Typography.Text>
              <StorageTags storage={storage} />
            </Space>
          </Descriptions.Item>
          {nfs && <Descriptions.Item label={t("overview.export")}><Typography.Text code>{storage.export}</Typography.Text></Descriptions.Item>}
          <Descriptions.Item label={t(nfs ? "overview.mountPoint" : "overview.path")}><Path value={storage.path} fallback="—" /></Descriptions.Item>
          {nfs && <Descriptions.Item label={t("overview.mountOptions")}><Path value={storage.mount_options} fallback="—" /></Descriptions.Item>}
          {storageTypeInfo(storage.type).cache && (
            <>
              <Descriptions.Item label={t("overview.cachePath")}><Path value={storage.cache_path} fallback="—" /></Descriptions.Item>
              <Descriptions.Item label={t("overview.cacheMax")}>{storage.cache_max_bytes ? formatBytes(storage.cache_max_bytes) : t("overview.cacheUnlimited")}</Descriptions.Item>
            </>
          )}
          <Descriptions.Item label={t("overview.scope")}>
            <Typography.Text>{t(storage.all_nodes ? "scope.all" : "overview.scopeChosen")}</Typography.Text>
            {storage.targets.length > 0 && <ul className="storage-target-list">{storage.targets.map((x, i) => <TargetLine key={`${x.node_uuid ?? x.label_key}-${i}`} target={x} />)}</ul>}
          </Descriptions.Item>
        </Descriptions>
      </DetailSection>
      <DetailSection title={t("overview.capacity")}>
        {shared ? (
          <Space direction="vertical" size={4} style={{ display: "flex" }}>
            <StorageCapacity total={storage.total_bytes} available={storage.available_bytes} label={t("overview.capacity")} />
            <Typography.Text type="secondary">{t("capacity.once")}</Typography.Text>
          </Space>
        ) : <LocalCapacity storage={storage} onShowNodes={onShowNodes} />}
      </DetailSection>
      {shared && (
        <DetailSection title={t("overview.controlPlanes")}>
          {/* Heartbeats are files on the storage: one this gpuworks cannot
              write carries none of its own and is never deleted from here. */}
          {storage.writable && storage.enabled
            ? <StorageControlPlanes storageId={storage.id} />
            : <Typography.Text type="secondary">{t("controlPlanes.readOnly")}</Typography.Text>}
        </DetailSection>
      )}
      <DetailSection title={t("overview.deployments")}>
        {storage.deployment_ids.length > 0
          ? <Space size={4} wrap>{storage.deployment_ids.map((id) => <Link key={id} to={`/deployments?deployment=${id}`}>#{id}</Link>)}</Space>
          : <Typography.Text type="secondary">{t("overview.noDeployments")}</Typography.Text>}
      </DetailSection>
    </>
  );
}

import { useEffect, useRef, useState, type MouseEvent } from "react";
import {
  Button,
  Card,
  Collapse,
  Descriptions,
  Input,
  Modal,
  Popconfirm,
  Select,
  Space,
  Table,
  Tag,
  Tooltip,
  Typography,
  message,
} from "antd";
import {
  CloudDownloadOutlined,
  CopyOutlined,
	DeleteOutlined,
  FolderAddOutlined,
  ReloadOutlined,
  RocketOutlined,
} from "@ant-design/icons";
import { formatDate } from "../i18n/datetime";
import { useTranslation } from "react-i18next";
import {
  api,
  type FSEntry,
  type Model,
  type ModelCore,
} from "../api";
import { isModelFileReady } from "../storageTypes";
import AIAssistant from "../components/AIAssistant";
import MarkdownContent from "../components/MarkdownContent";
import { formatBytes } from "../format";
import { refreshRemoteModelMetadata } from "./models/modelMetadataRefresh";
import { hasUnpinnedLocalCopy } from "./models/unpinnedRevision";
import { ModelFilesSection } from "./models/ModelFilesSection";
import { useLocalStorageChoice } from "./models/useStorages";
export { DownloadModal } from "./models/ModelDownloadModal";
import { inputModalitiesOf, inputModalityTypes, operationLabelKey, operationTypes, operationTypesOf } from "../modelInference";
import type { OperationType } from "../inferenceApi";
import { nodeDisplayName } from "../nodeDisplay";
import NodePicker from "../nodes/NodePicker";

export const sourceLabel: Record<string, string> = {
  huggingface: "source.huggingface",
  modelscope: "source.modelscope",
  local: "source.local",
  url: "source.url",
};

export function ModelSourceTag({ source }: { source: string }) {
  const { t } = useTranslation("models");
  const color = source === "huggingface" ? "gold" : source === "modelscope" ? "blue" : source === "url" ? "geekblue" : "default";
  const short = source === "huggingface" ? "HF" : source === "modelscope" ? "MS" : source === "url" ? "URL" : sourceLabel[source] ? t(sourceLabel[source]) : source;
  return <Tag color={color}>{short}</Tag>;
}

export const modalityLabel: Record<string, { labelKey: string; color: string }> = {
  text: { labelKey: "modality.text", color: "blue" },
  image: { labelKey: "modality.image", color: "magenta" },
  audio: { labelKey: "modality.audio", color: "cyan" },
  video: { labelKey: "modality.video", color: "purple" },
};


export function Modalities({ values }: { values?: string[] }) {
  const { t } = useTranslation("models");
  if (!values?.length) return <Typography.Text type="secondary">—</Typography.Text>;
  return (
    <Space size={[4, 4]} wrap>
      {values.map((value) => {
        const visual = modalityLabel[value];
        return <Tag key={value} color={visual?.color ?? "default"}>{visual ? t(visual.labelKey) : value}</Tag>;
      })}
    </Space>
  );
}

/** When the model was published at its source: the repository's creation, else its last update. */
export function modelPublishedAt(model: Pick<Model, "origin_created_at" | "origin_updated_at">): string | undefined {
  return model.origin_created_at || model.origin_updated_at;
}

export function modelDate(value?: string): string {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "—" : formatDate(date);
}

function modelInitial(model: Model): string {
  return (model.name || model.source_id || "M").trim().charAt(0).toUpperCase() || "M";
}

export function ModelLogo({ model, size = "default" }: { model: ModelCore; size?: "default" | "small" }) {
  const [failed, setFailed] = useState(false);
  useEffect(() => setFailed(false), [model.id, model.logo_path]);
  const className = `model-tile-logo${size === "small" ? " model-tile-logo-small" : ""}`;
  return (
    <span className={className} aria-hidden="true">
      {model.logo_path && !failed ? (
        <img src={`/api/v1/models/${model.id}/logo?v=${encodeURIComponent(model.logo_path)}`} alt="" onError={() => setFailed(true)} />
      ) : (
        <span className="model-logo-fallback">{modelInitial(model)}</span>
      )}
    </span>
  );
}

export function ModelTile({
  model,
  onOpen,
  onDownload,
  onCopy,
  onRegisterPath,
	onDelete,
}: {
  model: ModelCore;
  onOpen: () => void;
  onDownload: () => void;
  onCopy: () => void;
  onRegisterPath: () => void;
	onDelete: () => void;
}) {
  const { t } = useTranslation("models");
  const stop = (event: MouseEvent) => event.stopPropagation();
  return (
    <div className="model-tile" role="button" tabIndex={0} onClick={onOpen} onKeyDown={(event) => { if (event.key === "Enter" || event.key === " ") onOpen(); }}>
      <div className="model-tile-main">
        <ModelLogo model={model} />
        <div className="model-tile-copy">
          <Typography.Text className="model-tile-name" ellipsis={{ tooltip: model.name || model.source_id }}>{model.name || model.source_id}</Typography.Text>
          <div className="model-tile-tags">
            {operationTypesOf(model.operations).slice(0, 2).map((value) => <Tag color="blue" key={value}>{t(operationLabelKey[value])}</Tag>)}
            {inputModalitiesOf(model.input_modalities).slice(0, 2).map((value) => <Tag key={value}>{modalityLabel[value] ? t(modalityLabel[value].labelKey) : value}</Tag>)}
          </div>
        </div>
        <div className="model-tile-actions" onClick={stop}>
          <Tooltip title={t("action.deployInferenceTemplate")}><Button type="text" icon={<RocketOutlined />} onClick={() => location.assign(`/deploy?model=${model.id}`)} aria-label={t("action.deployInferenceTemplate")} /></Tooltip>
          {model.source !== "local" && <Tooltip title={t("action.downloadToNode")}><Button type="text" icon={<CloudDownloadOutlined />} onClick={onDownload} aria-label={t("action.downloadToNode")} /></Tooltip>}
          <Tooltip title={t("action.copyTo")}><Button type="text" icon={<CopyOutlined />} onClick={onCopy} aria-label={t("action.copyTo")} /></Tooltip>
          <Tooltip title={t("action.registerExistingPath")}><Button type="text" icon={<FolderAddOutlined />} onClick={onRegisterPath} aria-label={t("action.registerNodePath")} /></Tooltip>
		  <Popconfirm title={t("delete.confirm", { name: model.name || model.source_id })} description={t("delete.tileDescription")} okText={t("action.delete")} cancelText={t("action.cancel")} okButtonProps={{ danger: true }} onConfirm={onDelete}><Tooltip title={t("action.deleteModel")}><Button danger type="text" icon={<DeleteOutlined />} aria-label={t("action.deleteModel")} /></Tooltip></Popconfirm>
          <AIAssistant task="model.fit" subject={{ kind: "model", id: model.id, label: model.name || model.source_id }} buttonText="" buttonType="link" buttonSize="small" />
        </div>
      </div>
      <div className="model-tile-footer">
		<ModelSourceTag source={model.source} />
        <span className="model-tile-divider" />
        <span>{model.weight_gib ? `${model.weight_gib.toFixed(1)} GiB` : t("unknownSize")}</span>
        <span className="model-tile-divider" />
        <span>{modelDate(model.created_at)}</span>
      </div>
    </div>
  );
}

export function ModelDetailModal({
  model,
  onClose,
  focusFiles,
  onDownload,
  onCopy,
  onRegisterPath,
  onChanged,
}: {
  model: Model | null;
  onClose: () => void;
  /** Scrolls to the files section once the dialog shows (opened from the list's presence cell). */
  focusFiles?: boolean;
  onDownload: (model: Model) => void;
  onCopy: (model: Model) => void;
  onRegisterPath: (model: Model) => void;
  onChanged: (model?: Model) => void;
}) {
  const { t } = useTranslation("models");
  const [refreshing, setRefreshing] = useState(false);
  const modelId = model?.id;
  useEffect(() => {
    if (!focusFiles || modelId == null) return;
    // The dialog body mounts with the open animation; wait a frame for it.
    const timer = setTimeout(() => document.getElementById("model-files")?.scrollIntoView({ block: "start", behavior: "smooth" }), 120);
    return () => clearTimeout(timer);
  }, [focusFiles, modelId]);
  const [selectedOperations, setSelectedOperations] = useState<OperationType[]>([]);
  const [selectedInputModalities, setSelectedInputModalities] = useState<string[]>([]);
  const [savingInferenceConfig, setSavingInferenceConfig] = useState(false);
  // The models list is polled while downloads run, so model.operations and
  // model.input_modalities arrive as fresh array instances on every refresh.
  // Depend on their content, and never overwrite an in-progress edit: only
  // adopt the server value when switching models or when the selection is
  // still the previously loaded (clean) value.
  const serverOperations = operationTypesOf(model?.operations);
  const serverInputModalities = inputModalitiesOf(model?.input_modalities);
  const serverOperationsKey = serverOperations.join(",");
  const serverInputModalitiesKey = serverInputModalities.join(",");
  const loadedInferenceConfig = useRef<{ id?: number; operations: string; inputModalities: string }>({ operations: "", inputModalities: "" });
  useEffect(() => {
    const prev = loadedInferenceConfig.current;
    loadedInferenceConfig.current = { id: model?.id, operations: serverOperationsKey, inputModalities: serverInputModalitiesKey };
    const sameModel = prev.id === model?.id;
    setSelectedOperations((current) => (sameModel && current.join(",") !== prev.operations ? current : serverOperations));
    setSelectedInputModalities((current) => (sameModel && current.join(",") !== prev.inputModalities ? current : serverInputModalities));
    // serverOperations/serverInputModalities are fully described by their keys.
  }, [model?.id, serverOperationsKey, serverInputModalitiesKey]);
  const refreshMetadata = async () => {
    if (!model) return;
    setRefreshing(true);
    try {
      const updated = await refreshRemoteModelMetadata(model, api.refreshModelMetadata);
      message.success(t("detail.message.metadataUpdated"));
      onChanged(updated);
    } catch (e) {
      message.error(t("detail.message.refreshFailed", { detail: (e as Error).message }));
    } finally {
      setRefreshing(false);
    }
  };
  const saveInferenceConfig = async () => {
    if (!model) return;
    setSavingInferenceConfig(true);
    try {
      const updated = await api.updateModelInferenceConfig(model.id, selectedOperations.map((type) => ({ type })), selectedInputModalities);
      message.success(t("detail.message.inferenceConfigUpdated"));
      onChanged(updated);
    } catch (e) {
      message.error(t("detail.message.inferenceConfigFailed", { detail: (e as Error).message }));
    } finally {
      setSavingInferenceConfig(false);
    }
  };

  return (
    <Modal
      title={model?.name || model?.source_id || t("detail.title")}
      open={!!model}
      onCancel={onClose}
      width={720}
      footer={model ? [
        <Button key="close" onClick={onClose}>{t("action.close")}</Button>,
		<Button key="template" icon={<RocketOutlined />} onClick={() => location.assign(`/deploy?model=${model.id}`)}>{t("action.deployInferenceTemplate")}</Button>,
        model.source !== "local" ? <Button key="refresh" icon={<ReloadOutlined />} loading={refreshing} onClick={refreshMetadata}>{t("detail.refreshMetadata")}</Button> : null,
        <Button key="path" icon={<FolderAddOutlined />} onClick={() => onRegisterPath(model)}>{t("action.registerPath")}</Button>,
        <Button key="copy" icon={<CopyOutlined />} onClick={() => onCopy(model)}>{t("action.copyTo")}</Button>,
        model.source !== "local" ? <Button key="download" type="primary" icon={<CloudDownloadOutlined />} onClick={() => onDownload(model)}>{t("action.downloadToNode")}</Button> : null,
      ] : null}
    >
      {model && (
        <><div className="model-detail-heading"><ModelLogo model={model} /><div><Typography.Title level={4}>{model.name || model.source_id}</Typography.Title><Typography.Text type="secondary">{model.source_id}</Typography.Text></div></div><Descriptions bordered size="small" column={{ xs: 1, sm: 2 }}>
          <Descriptions.Item label={t("column.source")}>{sourceLabel[model.source] ? t(sourceLabel[model.source]) : model.source}</Descriptions.Item>
          <Descriptions.Item label={t("detail.revision")}>
            {model.revision || t("detail.defaultBranch")}
            {hasUnpinnedLocalCopy(model) && <Tooltip title={t("detail.unpinnedHint")}><Tag color="orange" style={{ marginLeft: 8 }}>{t("detail.unpinned")}</Tag></Tooltip>}
          </Descriptions.Item>
          <Descriptions.Item label={t("column.sourceID")} span={2}>{model.source_id}</Descriptions.Item>
          <Descriptions.Item label={t("detail.architecture")}>{model.arch || t("unrecognized")}</Descriptions.Item>
          <Descriptions.Item label={t("column.weight")}>{model.weight_gib ? `${model.weight_gib.toFixed(1)} GiB` : "—"}</Descriptions.Item>
          <Descriptions.Item label={t("detail.precision")}>{model.precision || t("unrecognized")}</Descriptions.Item>
          <Descriptions.Item label={t("detail.contextLength")}>{model.context_length ? model.context_length.toLocaleString() : t("unrecognized")}</Descriptions.Item>
          <Descriptions.Item label={t("detail.originCreatedAt")}>{modelDate(modelPublishedAt(model))}</Descriptions.Item>
          <Descriptions.Item label={t("detail.originUpdatedAt")}>{model.origin_updated_at ? modelDate(model.origin_updated_at) : "—"}</Descriptions.Item>
          <Descriptions.Item label={t("column.operations")} span={2}>
            <Select
              mode="multiple"
              aria-label={t("detail.operations")}
              style={{ width: "100%" }}
              value={selectedOperations}
              onChange={(values) => setSelectedOperations(values as OperationType[])}
              options={operationTypes.map((value) => ({ value, label: t(operationLabelKey[value]) }))}
              placeholder={t("detail.operationsPlaceholder")}
            />
          </Descriptions.Item>
          <Descriptions.Item label={t("column.inputModalities")} span={2}>
            <Select
              mode="multiple"
              aria-label={t("detail.inputModalities")}
              style={{ width: "100%" }}
              value={selectedInputModalities}
              onChange={(values) => setSelectedInputModalities(values)}
              options={inputModalityTypes.map((value) => ({ value, label: modalityLabel[value] ? t(modalityLabel[value].labelKey) : value }))}
              placeholder={t("detail.inputModalitiesPlaceholder")}
            />
          </Descriptions.Item>
          <Descriptions.Item label="" span={2}>
            <Button size="small" loading={savingInferenceConfig} onClick={saveInferenceConfig}>{t("action.save")}</Button>
          </Descriptions.Item>
          <Descriptions.Item label={t("detail.createdAt")}>{modelDate(model.created_at)}</Descriptions.Item>
          <Descriptions.Item label={t("files.count")}>{(model.nodes ?? []).filter((item) => !item.cache && isModelFileReady(item.state)).length + (model.storages ?? []).filter((item) => isModelFileReady(item.state)).length}</Descriptions.Item>
        </Descriptions></>
      )}
      {model && <Collapse size="small" ghost style={{ marginTop: 8 }} items={[{
        key: "advanced",
        label: t("detail.advancedInfo"),
        children: <Descriptions size="small" column={1}>
          <Descriptions.Item label={t("detail.requestedRevision")}>{model.revision || t("detail.defaultBranch")}</Descriptions.Item>
          <Descriptions.Item label={t("detail.nodePinnedRevision")}>
            {(model.nodes ?? []).some((item) => item.revision)
              ? <Space size={[4, 4]} wrap>{(model.nodes ?? []).filter((item) => item.revision).map((item) => <Tooltip key={item.node_uuid} title={item.node_uuid}><Tag>{nodeDisplayName({ uuid: item.node_uuid, name: item.node_name })} · {item.revision?.slice(0, 12)}</Tag></Tooltip>)}</Space>
              : t("detail.noResolvedRevision")}
          </Descriptions.Item>
        </Descriptions>,
      }]} />}
      {model && <ModelFilesSection model={model} onChanged={onChanged} />}
      {model?.readme && <Card size="small" title="README" style={{ marginTop: 16, maxHeight: 360, overflow: "auto" }}><MarkdownContent content={model.readme} allowHtml /></Card>}
    </Modal>
  );
}

export function RegisterPathModal({ model, onClose, onDone }: { model: Model | null; onClose: () => void; onDone: () => void }) {
  const { t } = useTranslation("models");
  const [nodeUuid, setNodeUuid] = useState<string>();
  const [path, setPath] = useState("");
  const [entries, setEntries] = useState<FSEntry[]>([]);
  const [listing, setListing] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [storageId, setStorageId] = useState<number>();
  const localStorages = useLocalStorageChoice(!!model);
  useEffect(() => { setStorageId(localStorages.defaultId); }, [localStorages.defaultId]);
  const reset = () => { setNodeUuid(undefined); setPath(""); setEntries([]); setStorageId(localStorages.defaultId); };
  const list = async () => {
    if (!nodeUuid || !path) return;
    setListing(true);
    try { const res = await api.nodeFSList(nodeUuid, path); setEntries(res.items); }
    catch (e) { message.error(t("path.listFailed", { detail: (e as Error).message })); }
    finally { setListing(false); }
  };
  const submit = async () => {
    if (!model || !nodeUuid || !path) return;
    setSubmitting(true);
    try {
      await (localStorages.choose && storageId ? api.registerModelPath(model.id, nodeUuid, path, storageId) : api.registerModelPath(model.id, nodeUuid, path));
      message.success(t("path.registered"));
      reset();
      onDone();
    } catch (e) {
      message.error(t("path.registerFailed", { detail: (e as Error).message }));
    } finally {
      setSubmitting(false);
    }
  };
  return <Modal title={t("path.title", { name: model?.name || model?.source_id || "" })} open={!!model} onCancel={() => { reset(); onClose(); }} onOk={submit} confirmLoading={submitting} okText={t("path.register")} cancelText={t("action.cancel")} okButtonProps={{ disabled: !nodeUuid || !path }} destroyOnHidden width={680}>
    <Space direction="vertical" size={12} style={{ width: "100%" }}>
      {localStorages.choose && <Select aria-label={t("register.storage")} style={{ width: "100%" }} value={storageId} onChange={setStorageId} options={localStorages.options.map((s) => ({ value: s.id, label: s.name }))} />}
      <NodePicker style={{ width: "100%" }} aria-label={t("detail.node")} placeholder={t("register.selectOnlineNode")} value={nodeUuid} onChange={(v) => { setNodeUuid(Array.isArray(v) ? v[0] : v); setEntries([]); }} filter={{ state: ["online"] }} optionLabel={(n) => `${nodeDisplayName(n)} · ${t("download.gpuCount", { count: n.gpu_count })}`} notFoundContent={t("download.noOnlineNodes")} />
      <Space.Compact style={{ width: "100%" }}><Input value={path} onChange={(e) => setPath(e.target.value)} placeholder="/models/Qwen/Qwen3-8B" /><Button loading={listing} onClick={list} disabled={!nodeUuid || !path}>{t("path.listDirectory")}</Button></Space.Compact>
      {entries.length > 0 && <Table<FSEntry> rowKey="name" size="small" pagination={false} dataSource={entries} columns={[{ title: t("column.name"), dataIndex: "name", render: (name: string, entry: FSEntry) => (entry.is_dir ? `${name}/` : name) }, { title: t("path.type"), dataIndex: "is_dir", width: 90, render: (isDir: boolean) => t(isDir ? "path.directory" : "path.file") }, { title: t("detail.size"), dataIndex: "size", width: 120, render: (size: number, entry: FSEntry) => (entry.is_dir ? "—" : formatBytes(size)) }]} />}
    </Space>
  </Modal>;
}


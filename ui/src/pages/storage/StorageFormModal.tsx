import { useEffect, useMemo, useRef, useState } from "react";
import { Alert, Button, Form, Input, Descriptions, Modal, Radio, Space, Steps, Tag, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type Node } from "../../api";
import { APIError } from "../../apiError";
import { STORAGE_TYPES, storageTypeInfo, type Storage, type StorageDefaults, type StorageProbeResult } from "../../storageTypes";
import { storageErrorText, storageTypeLabel } from "../../storageDisplay";
import StorageProbePanel from "./StorageProbePanel";
import StorageCacheDraftTable from "./StorageCacheDraftTable";
import StorageCachePolicyFields from "./StorageCachePolicyFields";
import { formatBytes } from "../../format";
import StorageTargetsEditor from "./StorageTargetsEditor";
import { useStorageCachePreview } from "./useStorageCachePreview";
import {
  buildCreateRequest, buildPatchRequest, buildProbeRequest, emptyForm, formFromStorage,
  probeNodeOf, probeSignature, validateForm, type StorageFormValues,
} from "./storageForm";

type ProbeState =
  | { status: "idle" }
  | { status: "running" }
  | { status: "done"; result: StorageProbeResult; signature: string }
  | { status: "error"; message: string };

/**
 * Add or edit a storage. The type is chosen first and the form follows it. A
 * network storage must pass a trial mount before it can be saved; a result
 * that is only readable saves as read-only (disabled).
 */
export default function StorageFormModal({ open, storage, nodes, onClose, onSaved }: {
  open: boolean;
  /** Set to edit that storage; absent to add one. */
  storage?: Storage | null;
  nodes: Node[];
  onClose: () => void;
  onSaved: (saved: Storage) => void;
}) {
  const { t } = useTranslation("storage");
  const editing = !!storage;
  const preview = useStorageCachePreview(open, storage);
  const [values, setValues] = useState<StorageFormValues>(() => emptyForm());
  const [baseline, setBaseline] = useState<StorageFormValues>(() => emptyForm());
  const [baselineUpdatedAt, setBaselineUpdatedAt] = useState<string>();
  const [step, setStep] = useState(0);
  const [probe, setProbe] = useState<ProbeState>({ status: "idle" });
  const [touched, setTouched] = useState(false);
  const [saving, setSaving] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  // The directories the server fills in for what the form leaves empty: shown
  // as placeholders, never sent.
  const [defaults, setDefaults] = useState<StorageDefaults | null>(null);
  useEffect(() => {
    if (!open || defaults) return;
    let alive = true;
    api.storageSettings().then((settings) => { if (alive) setDefaults(settings.defaults ?? null); }).catch(() => undefined);
    return () => { alive = false; };
  }, [open, defaults]);
  const named = (root?: string) => (root ? `${root}/${values.name.trim() || "<name>"}` : "");

  // The detail page refreshes its storage every few seconds: only opening the
  // form, or switching to another storage, starts it over, never a refresh.
  const storageId = storage?.id;
  const generation = useRef(0);
  useEffect(() => {
    if (!open) return;
    const initial = storage ? formFromStorage(storage) : emptyForm();
    setValues(initial);
    setBaseline(initial);
    setBaselineUpdatedAt(storage?.updated_at);
    setStep(0);
    setProbe({ status: "idle" });
    setTouched(false);
    setSaving(false);
    setSubmitError(null);
    // Requests already sent may still finish after cancel or switching resources.
    // Their server writes remain valid, but they no longer own this form session.
    return () => { generation.current += 1; };
  }, [open, storageId]);

  const patch = (next: Partial<StorageFormValues>) => setValues((current) => ({ ...current, ...next }));
  const errors = useMemo(() => validateForm(values, editing ? "edit" : "create"), [values, editing]);
  const hasErrors = Object.keys(errors).length > 0;
  const wizard = !editing && values.type === "nfs_cache";
  const network = storageTypeInfo(values.type).shared;
  const signature = useMemo(() => probeSignature(values), [values]);
  const probeCurrent = probe.status === "done" && probe.signature === signature;
  const probeOK = probeCurrent && probe.status === "done" && probe.result.ok;
  const mustProbe = !editing && network;
  const canSave = !saving && (!mustProbe || probeOK);
  const showError = (field: keyof typeof errors) => (touched && errors[field] ? t(`form.error.${errors[field]}`) : undefined);

  const runProbe = async () => {
    const currentGeneration = generation.current;
    setTouched(true);
    if (errors.name || errors.path || errors.export) return;
    setProbe({ status: "running" });
    try {
      const result = await api.probeStorage(buildProbeRequest(values));
      if (generation.current !== currentGeneration) return;
      setProbe({ status: "done", result, signature });
    } catch (e) {
      if (generation.current !== currentGeneration) return;
      setProbe({ status: "error", message: storageErrorText(t, e) });
    }
  };

  const save = async () => {
    const currentGeneration = generation.current;
    setTouched(true);
    if (hasErrors || !canSave || (wizard && step !== 3)) return;
    setSaving(true);
    setSubmitError(null);
    try {
      const changes = buildPatchRequest(values, baseline);
      if ((changes.targets || changes.all_nodes !== undefined) && baselineUpdatedAt) changes.expected_updated_at = baselineUpdatedAt;
      const saved = storage
        ? await api.updateStorage(storage.id, changes)
        : await api.createStorage(buildCreateRequest(values, probeNodeOf(values)));
      if (generation.current !== currentGeneration) return;
      if (!editing && !saved.enabled && network) message.warning(t("form.savedReadOnly", { name: saved.name }));
      else message.success(t(editing ? "form.updated" : "form.created", { name: saved.name }));
      onSaved(saved);
    } catch (e) {
      if (generation.current !== currentGeneration) return;
      const probeResult = e instanceof APIError && e.code === "STORAGE_PROBE_FAILED" ? (e.params?.probe as StorageProbeResult | undefined) : undefined;
      if (probeResult) setProbe({ status: "done", result: probeResult, signature });
      else setSubmitError(storageErrorText(t, e));
    } finally {
      if (generation.current === currentGeneration) setSaving(false);
    }
  };

  const next = () => {
    setTouched(true);
    const invalid = step === 0 ? errors.name || errors.export || errors.path : step === 1 ? errors.targets : errors.cachePath || errors.cacheMax || errors.overrides;
    if (!invalid) { setStep((current) => current + 1); setTouched(false); }
  };
  const defaultBytes = (values.cacheMaxGiB ?? 0) * 1024 ** 3;
  const defaultImpact = useMemo(() => {
    if (!preview.storage || values.cacheMaxGiB === baseline.cacheMaxGiB) return null;
    const targets = new Map(values.nodes.map((node) => [node.node_uuid, node]));
    const affected = (preview.storage.nodes.all ?? []).filter((node) => targets.get(node.node_uuid)?.cache_max_bytes == null);
    let over = 0, reclaimable = 0, protectedBytes = 0, unknown = 0, used = 0, totalReclaimable = 0, totalProtected = 0;
    for (const node of affected) {
      const ownPath = targets.get(node.node_uuid)?.cache_path;
      const nextPath = ownPath || values.cachePath;
      const changedPath = !nextPath || nextPath !== node.cache_path;
      if (!node.cache || node.state === "offline" || changedPath) { unknown++; continue; }
      used += node.cache.used_bytes;
      totalReclaimable += node.cache.reclaimable_bytes;
      totalProtected += node.cache.protected_bytes;
      if (defaultBytes > 0 && node.cache.used_bytes > defaultBytes) {
        over++;
        reclaimable += node.cache.reclaimable_bytes;
        protectedBytes += node.cache.protected_bytes;
      }
    }
    return { count: affected.length, own: (preview.storage.nodes.all?.length ?? 0) - affected.length, over, unknown, known: affected.length - unknown, used: formatBytes(used), totalReclaimable: formatBytes(totalReclaimable), totalProtected: formatBytes(totalProtected), reclaimable: formatBytes(reclaimable), protected: formatBytes(protectedBytes) };
  }, [preview.storage, values.nodes, values.cachePath, values.cacheMaxGiB, baseline, defaultBytes]);
  const readOnlyResult = probeOK && probe.status === "done" && !probe.result.writable;
  return (
    <Modal
      title={editing ? t("form.editTitle", { name: storage?.name }) : t("form.addTitle")}
      open={open}
      onCancel={onClose}
      width={storageTypeInfo(values.type).cache ? 1080 : 720}
      destroyOnHidden
      footer={[
        <Button key="cancel" onClick={onClose}>{t("form.cancel")}</Button>,
        wizard && step > 0 ? <Button key="back" onClick={() => setStep((current) => current - 1)}>{t("cacheDraft.back")}</Button> : null,
        network && (!wizard || step === 3) ? <Button key="probe" loading={probe.status === "running"} onClick={() => void runProbe()}>{t("form.probe")}</Button> : null,
        wizard && step < 3 ? <Button key="next" type="primary" onClick={next}>{t("cacheDraft.next")}</Button> : <Button key="save" type="primary" loading={saving} disabled={!canSave} onClick={() => void save()}>{t(readOnlyResult ? "form.saveReadOnly" : "form.save")}</Button>,
      ]}
    >
      {wizard && <Steps current={step} style={{ marginBottom: 24 }} items={["basic", "scope", "cache", "confirm"].map((key) => ({ title: t(`cacheDraft.step.${key}`) }))} />}
      <Form layout="vertical" onFinish={() => void save()}>
        {(!wizard || step === 0) && <>
        <Form.Item label={t("form.type")}>
          {editing ? (
            <Tag>{storageTypeLabel(t, values.type)}</Tag>
          ) : (
            <Radio.Group
              aria-label={t("form.type")}
              value={values.type}
              onChange={(e) => { patch({ type: e.target.value }); setProbe({ status: "idle" }); }}
              optionType="button"
              options={STORAGE_TYPES.map((type) => ({ value: type, label: storageTypeLabel(t, type) }))}
            />
          )}
          <Typography.Paragraph type="secondary" style={{ marginTop: 8, marginBottom: 0 }}>{t(`form.typeHelp.${values.type}`)}</Typography.Paragraph>
        </Form.Item>
        <Form.Item label={t("form.name")} htmlFor="storage-name" validateStatus={showError("name") ? "error" : undefined} help={showError("name") ?? t("form.nameHelp")}>
          <Input id="storage-name" value={values.name} onChange={(e) => patch({ name: e.target.value })} />
        </Form.Item>
        {storageTypeInfo(values.type).nfs && (
          <Form.Item label={t("form.export")} htmlFor="storage-export" validateStatus={showError("export") ? "error" : undefined} help={showError("export") ?? (editing ? t("form.exportLocked") : t("form.exportHelp"))}>
            <Input id="storage-export" value={values.export} disabled={editing} placeholder="nfs.example.internal:/export/models" onChange={(e) => patch({ export: e.target.value })} />
          </Form.Item>
        )}
        <Form.Item
          label={t(values.type === "local" ? "form.path" : values.type === "shared_dir" ? "form.sharedPath" : "form.mountPoint")}
          htmlFor="storage-path"
          validateStatus={showError("path") ? "error" : undefined}
          help={showError("path") ?? t(values.type === "local" ? "form.pathHelp" : values.type === "shared_dir" ? "form.sharedPathHelp" : "form.mountPointHelp", { mount: named(defaults?.mount_root) })}
        >
          <Input id="storage-path" value={values.path} placeholder={storageTypeInfo(values.type).nfs ? named(defaults?.mount_root) : values.type === "local" ? defaults?.local_path ?? "" : "/mnt/shared/models"} onChange={(e) => patch({ path: e.target.value })} />
        </Form.Item>
        {storageTypeInfo(values.type).nfs && (
          <Form.Item label={t("form.mountOptions")} htmlFor="storage-options" help={t("form.mountOptionsHelp")}>
            <Input id="storage-options" value={values.mountOptions} placeholder="vers=4.2,nconnect=4" onChange={(e) => patch({ mountOptions: e.target.value })} />
          </Form.Item>
        )}
        </>}
        {storageTypeInfo(values.type).cache && (!wizard || step === 2) && (
          <>
            <Form.Item label={t("form.cachePath")} htmlFor="storage-cache-path" validateStatus={showError("cachePath") ? "error" : undefined} help={showError("cachePath") ?? t("form.cachePathHelp")}>
              <Input id="storage-cache-path" value={values.cachePath} placeholder={defaults ? t("form.cachePathPlaceholder", { cache: named(defaults.cache_root) }) : ""} onChange={(e) => patch({ cachePath: e.target.value })} />
            </Form.Item>
            <Form.Item label={t("cacheDraft.defaultPolicy")} validateStatus={showError("cacheMax") ? "error" : undefined} help={showError("cacheMax") ?? t("cacheDraft.defaultHelp")}>
              <StorageCachePolicyFields id="storage-cache-max" label={t("form.cacheMax")} value={defaultBytes} onChange={(value) => patch({ cacheMaxGiB: (value ?? 0) / 1024 ** 3 })} />
            </Form.Item>
            {preview.loading && <Alert style={{ marginBottom: 16 }} type="info" showIcon message={t("cacheDraft.previewLoading")} />}
            {preview.error && <Alert style={{ marginBottom: 16 }} type="warning" showIcon message={t("cacheDraft.previewFailed")} description={storageErrorText(t, preview.error)} action={<Button onClick={preview.retry}>{t("detail.retry")}</Button>} />}
            {defaultImpact && <Alert style={{ marginBottom: 16 }} type={defaultImpact.over ? "warning" : "info"} showIcon
              message={t("cacheDraft.defaultImpact", defaultImpact)}
              description={<Space direction="vertical">{defaultImpact.known > 0 && <span>{t("cacheDraft.currentUsage", { ...defaultImpact, reclaimable: defaultImpact.totalReclaimable, protected: defaultImpact.totalProtected })}</span>}<span>{t("cacheDraft.trimImpact", defaultImpact)}</span><span>{t("cacheDraft.unknownImpact", defaultImpact)}</span></Space>} />}
            {editing && values.cachePath !== baseline.cachePath && <Alert type="info" showIcon message={t("cacheDraft.pathNotice")} />}
            <StorageCacheDraftTable values={values} nodes={nodes} storage={preview.storage ?? storage ?? undefined} onChange={patch} />
            {showError("overrides") && <Alert type="error" message={showError("overrides")} />}
          </>
        )}
        {(!wizard || step === 1) && <Form.Item label={t("form.scope.title")}>
          <StorageTargetsEditor values={values} nodes={nodes} errors={touched ? errors : {}} onChange={patch} />
        </Form.Item>}
        {wizard && step === 3 && <Descriptions column={1} bordered items={[
          { key: "name", label: t("form.name"), children: values.name },
          { key: "export", label: t("form.export"), children: values.export },
          { key: "scope", label: t("form.scope.title"), children: t(`form.scope.${values.scope}`) },
          { key: "path", label: t("form.cachePath"), children: values.cachePath || named(defaults?.cache_root) },
          { key: "limit", label: t("cacheDraft.defaultPolicy"), children: defaultBytes === 0 ? t("cachePolicy.unlimited") : formatBytes(defaultBytes) },
          { key: "own", label: t("cacheDraft.exceptions"), children: values.nodes.filter((node) => node.cache_path || node.cache_max_bytes != null).length },
        ]} />}
      </Form>
      <Space direction="vertical" style={{ width: "100%" }}>
        {probe.status === "running" && <Alert type="info" showIcon message={t("probe.running")} />}
        {probe.status === "error" && <Alert type="error" showIcon message={t("probe.requestFailed")} description={probe.message} />}
        {probe.status === "done" && <StorageProbePanel result={probe.result} />}
        {probe.status === "done" && !probeCurrent && <Alert type="warning" showIcon message={t("probe.stale")} />}
        {mustProbe && probe.status === "idle" && <Typography.Text type="secondary">{t("probe.required")}</Typography.Text>}
        {submitError && <Alert type="error" showIcon message={t("form.saveFailed")} description={submitError} />}
      </Space>
    </Modal>
  );
}

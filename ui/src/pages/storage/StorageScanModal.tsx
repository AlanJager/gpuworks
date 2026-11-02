import { useCallback, useEffect, useMemo, useState } from "react";
import { Alert, Button, Checkbox, Collapse, Modal, Space, Switch, Table, Tag, Tooltip, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { productMessageText } from "../../apiError";
import type { Storage, StorageImportItem, StorageImportResult, StorageScanCandidate, StorageScanSkipped } from "../../storageTypes";
import { storageErrorText } from "../../storageDisplay";
import { formatBytes } from "../../format";

const pageLimit = 200;
const basisKeys = ["marker", "modelscope", "huggingface", "path"];

function selectable(c: StorageScanCandidate): boolean {
  return !c.imported;
}

/** Candidates that are ticked by default: new and complete. An unfinished download needs a deliberate tick. */
function preselected(c: StorageScanCandidate): boolean {
  return !c.imported && !c.incomplete;
}

export function importItem(c: StorageScanCandidate): StorageImportItem {
  const item: StorageImportItem = { rel_path: c.rel_path, source: c.source, source_id: c.source_id, size_bytes: c.size_bytes };
  if (c.bind_model_id) item.model_id = c.bind_model_id;
  if (c.revision) item.revision = c.revision;
  if (c.subfolder) item.subfolder = c.subfolder;
  return item;
}

/**
 * Scan a shared storage and import what it holds. One node walks the tree page
 * by page; the user ticks directories and importing only writes this control
 * plane's database. Nothing is copied.
 */
export default function StorageScanModal({ storage, onClose, onImported }: {
  storage: Storage | null;
  onClose: () => void;
  onImported: () => void;
}) {
  const { t } = useTranslation("storage");
  const { t: tCommon } = useTranslation("common");
  const [candidates, setCandidates] = useState<StorageScanCandidate[]>([]);
  const [skipped, setSkipped] = useState<StorageScanSkipped[]>([]);
  const [cursor, setCursor] = useState<string | undefined>();
  const [complete, setComplete] = useState(false);
  const [scanning, setScanning] = useState(false);
  const [scanned, setScanned] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [onlyNew, setOnlyNew] = useState(false);
  const [importing, setImporting] = useState(false);
  const [result, setResult] = useState<StorageImportResult | null>(null);
  const storageId = storage?.id;

  const scan = useCallback(async (from?: string) => {
    if (!storageId) return;
    setScanning(true);
    setError(null);
    try {
      const page = await api.scanStorage(storageId, { cursor: from, limit: pageLimit });
      setCandidates((current) => {
        const known = new Set(current.map((c) => c.rel_path));
        return [...current, ...page.candidates.filter((c) => !known.has(c.rel_path))];
      });
      setSkipped((current) => [...current, ...page.skipped]);
      setSelected((current) => {
        const next = new Set(current);
        for (const c of page.candidates) if (preselected(c)) next.add(c.rel_path);
        return next;
      });
      setCursor(page.next_cursor || undefined);
      setComplete(page.complete);
      setScanned(true);
    } catch (e) {
      setError(storageErrorText(t, e));
    } finally {
      setScanning(false);
    }
  }, [storageId]);

  useEffect(() => {
    setCandidates([]);
    setSkipped([]);
    setCursor(undefined);
    setComplete(false);
    setScanned(false);
    setError(null);
    setSelected(new Set());
    setOnlyNew(false);
    setResult(null);
    if (storageId) void scan();
  }, [storageId, scan]);

  const rows = useMemo(() => (onlyNew ? candidates.filter((c) => !c.imported) : candidates), [candidates, onlyNew]);
  const selectableRows = useMemo(() => rows.filter(preselected), [rows]);
  const selectedCount = candidates.filter((c) => selected.has(c.rel_path)).length;
  const allSelected = selectableRows.length > 0 && selectableRows.every((c) => selected.has(c.rel_path));
  const toggleAll = (checked: boolean) => setSelected((current) => {
    const next = new Set(current);
    for (const c of selectableRows) {
      if (checked) next.add(c.rel_path);
      else next.delete(c.rel_path);
    }
    return next;
  });

  const doImport = async () => {
    if (!storage) return;
    const items = candidates.filter((c) => selected.has(c.rel_path) && selectable(c)).map(importItem);
    if (items.length === 0) return;
    setImporting(true);
    setError(null);
    try {
      const res = await api.importStorage(storage.id, items);
      setResult(res);
      const done = new Set(res.imported.map((i) => i.rel_path));
      setCandidates((current) => current.map((c) => (done.has(c.rel_path) ? { ...c, imported: true } : c)));
      setSelected((current) => new Set([...current].filter((p) => !done.has(p))));
      if (res.imported.length > 0) {
        message.success(t("scan.importedMessage", { count: res.imported.length }));
        onImported();
      }
    } catch (e) {
      setError(storageErrorText(t, e));
    } finally {
      setImporting(false);
    }
  };

  const statusTag = (c: StorageScanCandidate) => {
    if (c.imported) return <Tag color="green">{t("scan.status.imported")}</Tag>;
    if (c.incomplete) return <Tooltip title={c.incomplete_reason}><Tag color="orange">{t("scan.status.incomplete")}</Tag></Tooltip>;
    return <Tag color="blue">{t("scan.status.new")}</Tag>;
  };
  const skippedReason = (reason: string) => t(`scan.skipReason.${reason}`, { defaultValue: reason });

  return (
    <Modal
      title={t("scan.title", { name: storage?.name ?? "" })}
      open={!!storage}
      onCancel={onClose}
      width={980}
      destroyOnHidden
      footer={[
        <Button key="close" onClick={onClose}>{t("scan.close")}</Button>,
        <Button key="import" type="primary" loading={importing} disabled={selectedCount === 0} onClick={() => void doImport()}>{t("scan.import", { count: selectedCount })}</Button>,
      ]}
    >
      <Space direction="vertical" style={{ width: "100%" }} size={12}>
        <Typography.Text type="secondary">{t("scan.help")}</Typography.Text>
        {error && <Alert type="error" showIcon message={t("scan.failed")} description={error} action={!scanned ? <Button size="small" onClick={() => void scan()}>{t("scan.retry")}</Button> : undefined} />}
        {result && (
          <Alert
            type={result.failed.length > 0 ? "warning" : "success"}
            showIcon
            message={t("scan.result", { imported: result.imported.length, created: result.imported.filter((i) => i.created).length, failed: result.failed.length })}
            description={result.failed.length > 0 ? <ul style={{ margin: 0, paddingLeft: 18 }}>{result.failed.map((f) => <li key={f.rel_path}><Typography.Text code>{f.rel_path}</Typography.Text> {f.error_detail ? productMessageText(tCommon, f.error_detail) : f.error}</li>)}</ul> : undefined}
          />
        )}
        <Space wrap align="center">
          <Checkbox checked={allSelected} indeterminate={!allSelected && selectableRows.some((c) => selected.has(c.rel_path))} disabled={selectableRows.length === 0} onChange={(e) => toggleAll(e.target.checked)}>{t("scan.selectAll", { count: selectableRows.length })}</Checkbox>
          <Space size={6}><Switch size="small" aria-label={t("scan.onlyNew")} checked={onlyNew} onChange={setOnlyNew} /><span>{t("scan.onlyNew")}</span></Space>
          <Typography.Text type="secondary">{t("scan.found", { count: candidates.length })}</Typography.Text>
          <Button size="small" loading={scanning && !!cursor} disabled={!cursor || scanning} onClick={() => void scan(cursor)}>{t("scan.continue")}</Button>
          <Button size="small" disabled={scanning} onClick={() => { setCandidates([]); setSkipped([]); setSelected(new Set()); setResult(null); void scan(); }}>{t("scan.rescan")}</Button>
          {scanned && complete && !cursor && <Typography.Text type="secondary">{t("scan.complete")}</Typography.Text>}
        </Space>
        <Table<StorageScanCandidate>
          size="small"
          rowKey="rel_path"
          loading={scanning}
          dataSource={rows}
          locale={{ emptyText: scanned ? t("scan.empty") : t("scan.scanning") }}
          pagination={{ pageSize: 50, showSizeChanger: false, hideOnSinglePage: true }}
          rowSelection={{
            selectedRowKeys: [...selected],
            hideSelectAll: true,
            getCheckboxProps: (c) => ({ disabled: !selectable(c), "aria-label": t("scan.selectRow", { path: c.rel_path }) }),
            onSelect: (c, checked) => setSelected((current) => {
              const next = new Set(current);
              if (checked) next.add(c.rel_path);
              else next.delete(c.rel_path);
              return next;
            }),
          }}
          columns={[
            { title: t("scan.column.path"), dataIndex: "rel_path", render: (value: string) => <Typography.Text code>{value}</Typography.Text> },
            { title: t("scan.column.size"), dataIndex: "size_bytes", width: 100, render: (value: number) => formatBytes(value) },
            { title: t("scan.column.metadata"), key: "metadata", render: (_: unknown, c) => <Space direction="vertical" size={0}><Typography.Text>{c.format ?? "—"}</Typography.Text><Typography.Text type="secondary">{c.tasks?.length ? c.tasks.join(", ") : t("scan.unknownTask")}</Typography.Text>{c.metadata_issues?.length ? <Tooltip title={c.metadata_issues.map((code) => t(`scan.metadataIssue.${code}`, { defaultValue: code })).join("; ")}><Tag color="orange">{t("scan.metadataWarning")}</Tag></Tooltip> : null}</Space> },
            { title: t("scan.column.basis"), dataIndex: "basis", width: 150, render: (value: string) => (basisKeys.includes(value) ? t(`scan.basis.${value}`) : value) },
            { title: t("scan.column.bind"), key: "bind", width: 180, render: (_: unknown, c) => (c.bind_model_name ? <Typography.Text>{c.bind_model_name}</Typography.Text> : <Tag>{t("scan.newModel")}</Tag>) },
            { title: t("scan.column.status"), key: "status", width: 140, render: (_: unknown, c) => statusTag(c) },
          ]}
        />
        {skipped.length > 0 && (
          <Collapse size="small" items={[{
            key: "skipped",
            label: t("scan.skipped", { count: skipped.length }),
            children: <ul style={{ margin: 0, paddingLeft: 18 }}>{skipped.map((s) => <li key={`${s.rel_path}:${s.reason}`}><Typography.Text code>{s.rel_path}</Typography.Text> {skippedReason(s.reason)}</li>)}</ul>,
          }]} />
        )}
      </Space>
    </Modal>
  );
}

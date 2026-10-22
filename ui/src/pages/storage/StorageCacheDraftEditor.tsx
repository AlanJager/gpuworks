import { useState } from "react";
import { Alert, Button, Checkbox, Form, Input, Modal, Radio, Space } from "antd";
import { useTranslation } from "react-i18next";
import type { Storage } from "../../storageTypes";
import { formatBytes } from "../../format";
import StorageCachePolicyFields from "./StorageCachePolicyFields";
import { validCacheBytes, type NodeOverride } from "./storageForm";

/** One fixed draft selection; polling cannot add nodes to an open edit. */
export default function StorageCacheDraftEditor({ rows, defaultBytes, defaultPath, storage, onClose, onApply }: {
  rows: NodeOverride[];
  defaultBytes: number;
  defaultPath: string;
  storage?: Storage;
  onClose: () => void;
  onApply: (rows: NodeOverride[]) => void;
}) {
  const { t } = useTranslation("storage");
  const single = rows.length === 1;
  const [limitMode, setLimitMode] = useState(single ? "change" : "keep");
  const [limit, setLimit] = useState<number | undefined>(single ? rows[0].cache_max_bytes : undefined);
  const [pathMode, setPathMode] = useState(single ? (rows[0].cache_path ? "custom" : "inherit") : "keep");
  const [path, setPath] = useState(single ? rows[0].cache_path : "");
  const [mount, setMount] = useState(single ? rows[0].path : "");
  const [existing, setExisting] = useState(single && rows[0].existing);
  const [touched, setTouched] = useState(false);
  const invalidLimit = limitMode === "change" && !validCacheBytes(limit);
  const invalidPath = (pathMode === "custom" && !path.trim().startsWith("/")) || (single && !!mount.trim() && !mount.trim().startsWith("/"));
  const impact = (() => {
    const statuses = new Map(storage?.nodes.all?.map((node) => [node.node_uuid, node]));
    let over = 0, unknown = 0, used = 0, reclaimable = 0, protectedBytes = 0, totalReclaimable = 0, totalProtected = 0;
    for (const row of rows) {
      const node = statuses.get(row.node_uuid);
      const nextPath = pathMode === "keep" ? row.cache_path || defaultPath : pathMode === "inherit" ? defaultPath : path.trim();
      if (!node?.cache || node.state === "offline" || (nextPath && nextPath !== node.cache_path)) { unknown++; continue; }
      used += node.cache.used_bytes;
      totalReclaimable += node.cache.reclaimable_bytes;
      totalProtected += node.cache.protected_bytes;
      const nextLimit = (limitMode === "change" ? limit : row.cache_max_bytes) ?? defaultBytes;
      if (nextLimit > 0 && node.cache.used_bytes > nextLimit) {
        over++;
        reclaimable += node.cache.reclaimable_bytes;
        protectedBytes += node.cache.protected_bytes;
      }
    }
    return { over, unknown, used: formatBytes(used), totalReclaimable: formatBytes(totalReclaimable), totalProtected: formatBytes(totalProtected), reclaimable: formatBytes(reclaimable), protected: formatBytes(protectedBytes) };
  })();
  const apply = () => {
    setTouched(true);
    if (invalidLimit || invalidPath) return;
    onApply(rows.map((row) => ({ ...row,
      ...(limitMode === "change" ? { cache_max_bytes: limit } : {}),
      ...(pathMode !== "keep" ? { cache_path: pathMode === "inherit" ? "" : path.trim() } : {}),
      ...(single ? { path: mount.trim(), existing } : {}),
    })));
  };
  return <Modal open title={t("cacheDraft.editTitle", { count: rows.length })} onCancel={onClose} destroyOnHidden footer={<Space><Button onClick={onClose}>{t("form.cancel")}</Button><Button type="primary" onClick={apply}>{t("cacheDraft.apply")}</Button></Space>}>
    <Form layout="vertical">
      <Form.Item label={t("form.cachePath")}>
        <div role="radiogroup" aria-label={t("cacheDraft.pathMode")}><Radio.Group value={pathMode} onChange={(e) => setPathMode(e.target.value)}>
          {!single && <Radio value="keep">{t("cacheDraft.keep")}</Radio>}
          <Radio value="inherit">{t("cachePolicy.inherit")}</Radio><Radio value="custom">{t("cachePolicy.custom")}</Radio>
        </Radio.Group></div>
        {pathMode === "custom" && <Input aria-label={t("form.cachePath")} value={path} onChange={(e) => setPath(e.target.value)} />}
      </Form.Item>
      <Form.Item label={t("cachePolicy.limit")}>
        {!single && <Checkbox checked={limitMode === "change"} onChange={(e) => setLimitMode(e.target.checked ? "change" : "keep")}>{t("cacheDraft.changeLimit")}</Checkbox>}
        {limitMode === "change" && <StorageCachePolicyFields value={limit} onChange={setLimit} allowInherit defaultBytes={defaultBytes} />}
      </Form.Item>
      {single && <>
        <Form.Item label={t("form.mountPoint")}><Input aria-label={t("form.mountPoint")} placeholder={t("form.scope.pathOverridePlaceholder")} value={mount} onChange={(e) => setMount(e.target.value)} /></Form.Item>
        <Checkbox checked={existing} onChange={(e) => setExisting(e.target.checked)}>{t("cacheDraft.existing")}</Checkbox>
      </>}
    </Form>
    <Space direction="vertical" style={{ width: "100%", marginTop: 16 }}>
      {touched && invalidLimit && <Alert type="error" showIcon message={t("form.error.cacheLimit")} />}
      {touched && invalidPath && <Alert type="error" showIcon message={t("form.error.pathAbsolute")} />}
      {storage && <Alert type={impact.over ? "warning" : "info"} showIcon message={impact.unknown === rows.length ? t("cacheDraft.unknownImpact", impact) : t("cacheDraft.currentUsage", { ...impact, reclaimable: impact.totalReclaimable, protected: impact.totalProtected })} description={impact.unknown < rows.length && <Space direction="vertical"><span>{t("cacheDraft.trimImpact", impact)}</span><span>{t("cacheDraft.unknownImpact", impact)}</span></Space>} />}
      <Alert type="info" showIcon message={t("cacheDraft.pathNotice")} />
    </Space>
  </Modal>;
}

import { Button, Checkbox, Input, Radio, Select, Space, Typography } from "antd";
import { DeleteOutlined, PlusOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import type { Node } from "../../api";
import { nodeDisplayName } from "../../nodeDisplay";
import { storageTypeInfo } from "../../storageTypes";
import type { NodeOverride, NodeScope, StorageFormErrors, StorageFormValues } from "./storageForm";

/**
 * Which nodes a storage applies to: all of them (with the nodes that use a
 * directory of their own), the ones carrying a label, or
 * chosen one by one, each with its own path override and, for NFS, the choice
 * to use a mount that is already there. A storage the server holds with several
 * of those at once shows all of them together.
 */
export default function StorageTargetsEditor({ values, nodes, errors, onChange }: {
  values: StorageFormValues;
  nodes: Node[];
  errors: StorageFormErrors;
  onChange: (patch: Partial<StorageFormValues>) => void;
}) {
  const { t } = useTranslation("storage");
  const nodeName = (uuid: string) => {
    const node = nodes.find((item) => item.uuid === uuid);
    return node ? nodeDisplayName(node) : uuid.slice(0, 8);
  };
  const setNode = (uuid: string, patch: Partial<NodeOverride>) =>
    onChange({ nodes: values.nodes.map((n) => (n.node_uuid === uuid ? { ...n, ...patch } : n)) });
  const cache = storageTypeInfo(values.type).cache;
  const pickNodes = (uuids: string[]) => {
    const picked = new Set(uuids);
    const existing = new Map(values.nodes.map((node) => [node.node_uuid, node]));
    // Removing membership keeps configured directories and limits as inactive exceptions.
    const preserved = values.nodes.filter((node) => !picked.has(node.node_uuid) && (
      node.override_only || (cache && (node.path || node.cache_path || node.cache_max_bytes != null || node.existing))
    )).map((node) => ({ ...node, override_only: true }));
    const selected = uuids.map((uuid) => ({
      ...(existing.get(uuid) ?? { node_uuid: uuid, path: "", cache_path: "", existing: false }),
      override_only: cache && values.scope === "all" ? (existing.get(uuid)?.override_only ?? true) : false,
    }));
    onChange({ nodes: [...preserved, ...selected],
      ...(values.scope === "mixed" && !values.allNodes && !selected.length ? { scope: "labels" as const } : {}),
    });
  };
  const setLabel = (index: number, patch: Partial<{ key: string; value: string }>) =>
    onChange({ labels: values.labels.map((l, i) => (i === index ? { ...l, ...patch } : l)) });
  const overrides = cache ? null : values.nodes.map((node) => (
    <div key={node.node_uuid} className="storage-node-override">
      <Typography.Text strong>{nodeName(node.node_uuid)}</Typography.Text>
      <Space wrap style={{ marginTop: 4 }}>
        <Input style={{ width: 260 }} aria-label={t("form.scope.pathOverride", { node: nodeName(node.node_uuid) })} placeholder={t("form.scope.pathOverridePlaceholder")} value={node.path} onChange={(e) => setNode(node.node_uuid, { path: e.target.value })} />
        {storageTypeInfo(values.type).cache && (
          <Input style={{ width: 260 }} aria-label={t("form.scope.cachePathOverride", { node: nodeName(node.node_uuid) })} placeholder={t("form.scope.cachePathOverridePlaceholder")} value={node.cache_path} onChange={(e) => setNode(node.node_uuid, { cache_path: e.target.value })} />
        )}
        {storageTypeInfo(values.type).nfs && (
          <Checkbox checked={node.existing} onChange={(e) => setNode(node.node_uuid, { existing: e.target.checked })}>{t("form.scope.existing", { node: nodeName(node.node_uuid) })}</Checkbox>
        )}
      </Space>
    </div>
  ));
  const nodePicker = (label: string, allowClear: boolean) => (
    <Select
      mode="multiple"
      allowClear={allowClear}
      style={{ width: "100%" }}
      aria-label={label}
      placeholder={label}
      value={values.nodes.filter((n) => !cache || !n.override_only).map((n) => n.node_uuid)}
      onChange={pickNodes}
      optionFilterProp="label"
      options={nodes.map((node) => ({ value: node.uuid, label: nodeDisplayName(node) }))}
    />
  );
  const labelRows = (
    <>
      {values.labels.map((label, index) => (
        <Space key={index} align="start">
          <Input aria-label={t("form.scope.labelKey")} placeholder={t("form.scope.labelKey")} value={label.key} onChange={(e) => setLabel(index, { key: e.target.value })} />
          <Input aria-label={t("form.scope.labelValue")} placeholder={t("form.scope.labelValue")} value={label.value} onChange={(e) => setLabel(index, { value: e.target.value })} />
          <Button type="text" icon={<DeleteOutlined />} aria-label={t("form.scope.removeLabel")} disabled={values.labels.length <= 1} onClick={() => onChange({ labels: values.labels.filter((_, i) => i !== index) })} />
        </Space>
      ))}
      <Button size="small" icon={<PlusOutlined />} onClick={() => onChange({ labels: [...values.labels, { key: "", value: "" }] })}>{t("form.scope.addLabel")}</Button>
    </>
  );
  return (
    <div>
      <Radio.Group
        aria-label={t("form.scope.title")}
        value={values.scope}
        onChange={(event) => {
          const scope = event.target.value as NodeScope;
          onChange(scope === "labels" && values.nodes.some((node) => !node.override_only)
            ? { scope: "mixed", allNodes: false }
            : { scope });
        }}
        options={[
          { value: "all", label: t("form.scope.all") },
          { value: "labels", label: t("form.scope.labels") },
          { value: "nodes", label: t("form.scope.nodes") },
          // A storage held with several kinds of target keeps them all; this choice only exists while it is one.
          ...(values.scope === "mixed" ? [{ value: "mixed", label: t("form.scope.mixed") }] : []),
        ]}
        optionType="button"
      />
      {values.scope === "all" && (
        <Space direction="vertical" style={{ marginTop: 8, width: "100%" }}>
          <Typography.Text type="secondary">{t("form.scope.allHelp")}</Typography.Text>
          {!cache && nodePicker(t("form.scope.overrideNodes"), true)}
          {!cache && overrides}
        </Space>
      )}
      {values.scope === "labels" && (
        <Space direction="vertical" style={{ marginTop: 8, width: "100%" }}>
          <Typography.Text type="secondary">{t("form.scope.labelsHelp")}</Typography.Text>
          {labelRows}
        </Space>
      )}
      {values.scope === "nodes" && (
        <Space direction="vertical" style={{ marginTop: 8, width: "100%" }}>
          {nodePicker(t("form.scope.chooseNodes"), false)}
          {!cache && overrides}
        </Space>
      )}
      {values.scope === "mixed" && (
        <Space direction="vertical" style={{ marginTop: 8, width: "100%" }}>
          <Typography.Text type="secondary">{t("form.scope.mixedHelp")}</Typography.Text>
          <Checkbox checked={values.allNodes} onChange={(e) => onChange({ allNodes: e.target.checked })}>{t("form.scope.mixedAll")}</Checkbox>
          {labelRows}
          {nodePicker(t(values.allNodes ? "form.scope.overrideNodes" : "form.scope.chooseNodes"), true)}
          {!cache && overrides}
        </Space>
      )}
      {errors.targets && <Typography.Text type="danger" style={{ display: "block", marginTop: 4 }}>{t(`form.error.${errors.targets}`)}</Typography.Text>}
      {errors.overrides && <Typography.Text type="danger" style={{ display: "block", marginTop: 4 }}>{t(`form.error.${errors.overrides}`)}</Typography.Text>}
    </div>
  );
}

import { useEffect, useMemo, useState } from "react";
import { Alert, AutoComplete, Card, Form, Input, Modal, Radio, message } from "antd";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import NodePicker from "../../nodes/NodePicker";
import { useNodeLookup } from "../../nodes/useNodeLookup";
import { nodeDisplayName } from "../../nodeDisplay";
import type { NetworkAttachment, NetworkDetail, NodeNetworks } from "../../networkTypes";
import { usesRails } from "./networkDisplay";

interface FormValues {
  mode: "node" | "label";
  node_uuids?: string[];
  label_key?: string;
  label_value?: string;
  interface?: string;
  ip?: string;
}

/**
 * Attaches a network to nodes (several at once) or a label, or, with
 * `attachment` set, edits that node attachment's address override: the data
 * address on Ethernet, the bootstrap address on RDMA. An RDMA network's rails
 * are edited in the node table, not here.
 */
export default function AttachNetworkModal({
  open, network, attachment, onClose, onSaved,
}: {
  open: boolean;
  network: NetworkDetail;
  attachment?: NetworkAttachment | null;
  onClose: () => void;
  onSaved: (detail: NetworkDetail) => void;
}) {
  const { t } = useTranslation("networks");
  const [form] = Form.useForm<FormValues>();
  const [inventory, setInventory] = useState<NodeNetworks | null>(null);
  const [saving, setSaving] = useState(false);
  const mode = Form.useWatch("mode", form);
  const editing = !!attachment;
  const rdma = usesRails(network.type);

  useEffect(() => {
    if (!open) return;
    form.resetFields();
    form.setFieldsValue(attachment
      ? { mode: "node", node_uuids: attachment.node_uuid ? [attachment.node_uuid] : [], interface: attachment.interface, ip: attachment.ip }
      : { mode: "node", node_uuids: [] });
  }, [open, attachment, form]);

  useEffect(() => {
    setInventory(null);
    if (!open || !attachment?.node_uuid) return;
    let cancelled = false;
    api.nodeNetworks(attachment.node_uuid).then((inv) => { if (!cancelled) setInventory(inv); }).catch(() => undefined);
    return () => { cancelled = true; };
  }, [open, attachment]);

  const interfaceOptions = useMemo(() => (inventory?.interfaces ?? []).map((i) => ({
    value: i.name,
    label: [i.name, ...i.addrs].join(" · "),
  })), [inventory]);
  const attached = new Set(network.attachments.filter((a) => a.target === "node").map((a) => a.node_uuid));
  // The picker searches nodes on the server; the ones picked are read back
  // to name them in the outcome.
  const picked = useNodeLookup((Form.useWatch("node_uuids", form) as string[] | undefined) ?? []);
  const nodeName = (uuid: string) => {
    const n = picked.get(uuid);
    return n ? nodeDisplayName(n) : uuid;
  };

  const submit = async () => {
    let v: FormValues;
    try {
      v = await form.validateFields();
    } catch {
      return; // the form shows the field errors
    }
    setSaving(true);
    try {
      if (attachment) {
        onSaved(await api.updateNetworkAttachment(network.id, attachment.id, { interface: v.interface?.trim() ?? "", ip: v.ip?.trim() ?? "", hcas: attachment.hcas }));
        message.success(t("message.saved"));
      } else if (v.mode === "node") {
        const out = await api.attachNetworkNodes(network.id, v.node_uuids ?? []);
        const failed = out.results.filter((r) => r.status === "failed");
        if (failed.length > 0) {
          message.warning(t("attach.partial", {
            ok: out.results.length - failed.length,
            failed: failed.map((r) => `${nodeName(r.node_uuid)}（${r.error ?? r.code}）`).join("、"),
          }));
        } else {
          message.success(t("attach.done", { count: out.results.length }));
        }
        onSaved(out.network);
      } else {
        onSaved(await api.attachNetwork(network.id, { label_key: v.label_key?.trim(), label_value: v.label_value?.trim() ?? "" }));
        message.success(t("message.saved"));
      }
    } catch (e) {
      message.error(t("message.failed", { detail: (e as Error).message }));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      title={editing ? t(rdma ? "attach.editBootstrapTitle" : "attach.editTitle") : t("attach.title", { name: network.name })}
      open={open}
      onCancel={onClose}
      onOk={submit}
      confirmLoading={saving}
      okText={t("actions.save")}
      cancelText={t("actions.cancel")}
      destroyOnClose
      width={620}
    >
      <Form form={form} layout="vertical">
        {!editing && (
          <Form.Item label={t("attach.mode")} name="mode" extra={t("attach.modeHint")}>
            <Radio.Group optionType="button" options={[
              { value: "node", label: t("attach.modeNode") },
              { value: "label", label: t("attach.modeLabel") },
            ]} />
          </Form.Item>
        )}
        {mode === "label" ? (
          <>
            <Form.Item label={t("attach.labelKey")} name="label_key" rules={[{ required: true, whitespace: true, message: t("attach.labelKeyRequired") }]}>
              <Input placeholder="pool" />
            </Form.Item>
            <Form.Item label={t("attach.labelValue")} name="label_value">
              <Input placeholder="h20-a" />
            </Form.Item>
          </>
        ) : (
          <>
            <Form.Item
              label={t("attach.nodes")}
              name="node_uuids"
              extra={!editing && rdma ? t("attach.rdmaInferred") : undefined}
              rules={[{ required: true, type: "array", min: 1, message: t("attach.nodeRequired") }]}
            >
              <NodePicker
                mode="multiple"
                disabled={editing}
                aria-label={t("attach.nodes")}
                optionDisabled={(n) => !editing && attached.has(n.uuid)}
              />
            </Form.Item>
            {editing && (
              <Card size="small" title={t(rdma ? "attach.bootstrapOverride" : "attach.overrides")}>
                <Form.Item label={rdma ? t("attach.interfaceIB") : t("attach.interface")} name="interface">
                  <AutoComplete allowClear options={interfaceOptions} filterOption={(input, option) => String(option?.value ?? "").includes(input)} />
                </Form.Item>
                <Form.Item label={t("attach.ip")} name="ip" style={{ marginBottom: 0 }}>
                  <Input placeholder={t("attach.ipPlaceholder")} />
                </Form.Item>
              </Card>
            )}
          </>
        )}
        {mode === "label" && <Alert style={{ marginTop: 8 }} type="info" showIcon message={t("attach.modeHint")} />}
      </Form>
    </Modal>
  );
}

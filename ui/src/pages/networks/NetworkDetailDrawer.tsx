import { useCallback, useEffect, useMemo, useState } from "react";
import { Descriptions, Typography } from "antd";
import NetworkLabelsDialog from "./NetworkLabelsDialog";
import NetworkLoadTargets from "./NetworkLoadTargets";
import { Button } from "../../components/ui/button";
import { ConfirmPopover } from "../../ui-kit/ConfirmPopover";
import { Notice } from "../../ui-kit/Notice";
import { Spinner } from "../../ui-kit/Spinner";
import { message } from "../../ui-kit/feedback";
import NetworkTargetTable from "./NetworkTargetTable";
import type { ResourceColumn } from "../../components/resourceTable/ResourceTable";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import type { NetworkAttachment, NetworkDetail, NetworkNode } from "../../networkTypes";
import DetailDrawer from "../../components/detail/DetailDrawer";
import DetailSection from "../../components/detail/DetailSection";
import { formatTime } from "../../i18n/datetime";
import AttachNetworkModal from "./AttachNetworkModal";
import NetworkFormModal from "./NetworkFormModal";
import NetworkTestPanel from "./NetworkTestPanel";
import NetworkMonitorPanel from "./NetworkMonitorPanel";
import ManagementNodesTable from "./ManagementNodesTable";
import { Endpoint, LinkLayerTag, RoleTags, TypeTag, WarningMark, isAcceleratorRDMA, usesRails, sourceText } from "./networkDisplay";
import { BootstrapCell, RailsCell } from "./RDMAEndpoints";
import { HelpTip } from "../../ui-kit/HelpTip";

const tabKeys = ["overview", "monitor", "test"] as const;

type Props = {
  // The network whose detail is open, if any (the list page's ?network=).
  id: number | null;
  tab?: string;
  onTab: (key: string) => void;
  onClose: () => void;
  // Something the list shows changed (renamed, deleted).
  onChanged: () => void;
};

// antd's drawer slides out in 0.3 s (motionDurationSlow).
const SLIDE_OUT_MS = 400;

/** One network in a drawer over the list: where it applies and which nodes it resolved to, its test, its traffic. */
export default function NetworkDetailDrawer(props: Props) {
  // Reading lives with a shown drawer only. A closing drawer keeps the last
  // network while it slides out, then unmounts and reads nothing.
  const [shown, setShown] = useState<number | null>(props.id);
  if (props.id != null && props.id !== shown) setShown(props.id);
  useEffect(() => {
    if (props.id != null) return;
    const timer = setTimeout(() => setShown(null), SLIDE_OUT_MS);
    return () => clearTimeout(timer);
  }, [props.id]);
  const id = props.id ?? shown;
  if (id == null) return <DetailDrawer open={false} onClose={props.onClose} title="" />;
  return <OpenNetworkDetail {...props} id={id} open={props.id != null} />;
}

function OpenNetworkDetail({ id, open, tab: requested, onTab, onClose, onChanged }: Omit<Props, "id"> & { id: number; open: boolean }) {
  const { t } = useTranslation("networks");
  const [detail, setDetail] = useState<NetworkDetail | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [attachOpen, setAttachOpen] = useState(false);
  const [editingAttachment, setEditingAttachment] = useState<NetworkAttachment | null>(null);
  const [formOpen, setFormOpen] = useState(false);
  const tab = tabKeys.find((key) => key === requested) ?? "overview";

  const load = useCallback(() => {
    api.network(id).then((d) => { setDetail(d); setError(null); }).catch((e: Error) => setError(e));
  }, [id]);
  useEffect(load, [load]);
  // A running test finishes on the server; poll until its result arrives.
  const testRunning = detail?.latest_test?.status === "running";
  useEffect(() => {
    if (!testRunning) return;
    const timer = setInterval(load, 2000);
    return () => clearInterval(timer);
  }, [testRunning, load]);

  const current = detail?.id === id ? detail : null;
  if (!current) {
    return (
      <DetailDrawer open={open} onClose={onClose} title={t("detail.title")}>
        {error ? <Notice tone="danger" title={error.message} action={<Button variant="secondary" size="sm" onClick={load}>{t("detail.retry")}</Button>} /> : <Spinner label={t("detail.title")} />}
      </DetailDrawer>
    );
  }
  const name = current.builtin ? t("management.name") : current.name;
  // A change made in the drawer can change what the list shows (a network's
  // attachments, its resolved nodes): the list reads again with it.
  const updated = (next: NetworkDetail) => { setDetail(next); onChanged(); };
  const testTab = { key: "test", label: t("tabs.test"), children: <NetworkTestPanel network={current} onStarted={load} onChanged={current.builtin ? () => load() : setDetail} /> };
  const monitorTab = { key: "monitor", label: t("tabs.monitor"), children: <NetworkMonitorPanel network={current} /> };

  const remove = async () => {
    try {
      await api.deleteNetwork(current.id);
      message.success(t("message.deleted"));
      onChanged();
      onClose();
    } catch (e) {
      message.error(t("message.failed", { detail: (e as Error).message }));
    }
  };

  return (
    <DetailDrawer
      open={open}
      onClose={onClose}
      title={name}
      subtitle={<span className="inline-flex flex-wrap gap-1"><RoleTags roles={current.roles} /><TypeTag type={current.type} /></span>}
      // The management network cannot be edited or deleted; what changes is
      // which roles it carries on each node (its node table).
      extra={
        current.builtin ? undefined : (
          <span className="inline-flex items-center gap-2">
            <Button variant="secondary" size="sm" onClick={() => setFormOpen(true)}>{t("actions.edit")}</Button>
            <ConfirmPopover title={t("actions.confirmDelete", { name: current.name })} onConfirm={remove} confirmText={t("actions.delete")} cancelText={t("actions.cancel")} destructive trigger={<Button size="sm" variant="destructive">{t("actions.delete")}</Button>} />
          </span>
        )
      }
      activeTab={tab}
      onTabChange={onTab}
      tabs={[
        {
          key: "overview",
          label: t("tabs.overview"),
          children: current.builtin ? (
            <>
              {/* What the network is, point by point; what its switches do is said beside them (ManagementNodesTable). */}
              <Notice className="mb-4" title={t("management.intro.title")}><ul className="detail-points">{(["source", "fixed", "roles"] as const).map((key) => <li key={key}>{t(`management.intro.${key}`)}</li>)}</ul></Notice>
              <DetailSection title={t("detail.resolved", { count: current.nodes.length })}>
                <ManagementNodesTable nodes={current.nodes} onChanged={load} />
              </DetailSection>
            </>
          ) : (
            <NetworkOverview
              key={current.id}
              detail={current}
              description={current.description}
              onDetail={updated}
              onAttach={(attachment) => { setEditingAttachment(attachment); setAttachOpen(true); }}
            />
          ),
        },
        monitorTab,
        testTab,
      ]}
    >
      {!current.builtin && (
        <>
          <AttachNetworkModal
            open={attachOpen}
            network={current}
            attachment={editingAttachment}
            onClose={() => setAttachOpen(false)}
            onSaved={(d) => { setAttachOpen(false); updated(d); }}
          />
          <NetworkFormModal
            open={formOpen}
            network={current}
            roleOptions={current.role_options}
            typeOptions={current.type_options}
            typeRoles={current.type_roles}
            onClose={() => setFormOpen(false)}
            onSaved={(d) => { setFormOpen(false); updated(d); }}
          />
        </>
      )}
    </DetailDrawer>
  );
}

/** A defined network's overview: how it picks addresses, what it is attached to, and the nodes it resolved to. */
function NetworkOverview({ detail, description, onDetail, onAttach }: {
  detail: NetworkDetail;
  description?: string;
  onDetail: (detail: NetworkDetail) => void;
  onAttach: (attachment: NetworkAttachment | null) => void;
}) {
  const { t } = useTranslation("networks");
  const rails = usesRails(detail.type);
  const [labelEditor, setLabelEditor] = useState<{ attachment?: NetworkAttachment } | null>(null);
  const attachments = useMemo(() => new Map(detail.attachments.map(a => [a.id, a])), [detail.attachments]);
  const detach = async (attachment: NetworkAttachment) => {
    try {
      onDetail(await api.detachNetwork(detail.id, attachment.id));
    } catch (e) {
      message.error(t("message.failed", { detail: (e as Error).message }));
    }
  };
  const nodeColumns: ResourceColumn<NetworkNode>[] = [
    {
      title: t("nodeTable.node"),
      dataIndex: "node_name", key: "node", width: 140, role: "name",
      render: (_, n) => <span><Link to="/nodes" state={{ nodeUUID: n.node_uuid }} className="detail-name-cap" title={n.node_name} onClick={(event) => event.stopPropagation()}>{n.node_name}</Link><WarningMark warnings={n.warnings} /></span>,
    },
    {
      key: "endpointSummary", title: t(rails ? "nodeTable.bootstrapPorts" : "nodeTable.endpoint"), width: 220,
      render: (_, n) => rails
        ? <span>{n.ip || t("rail.noBootstrap")} · {t(isAcceleratorRDMA(detail.type) ? "nodeTable.cardCount" : "nodeTable.portCount", { count: n.hcas.length })}</span>
        : <Endpoint iface={n.interface} ip={n.ip} />,
    },
    {
      title: t("nodeTable.roles"), dataIndex: "roles", key: "roles", width: 130,
      filters: detail.roles.map(role => ({ value: role, text: t(`role.${role}`, { defaultValue: role }) })),
      render: (_, n) => <RoleTags roles={n.roles} />,
    },
    {
      title: t("nodeTable.source"), dataIndex: "source", key: "source", width: 150,
      filters: [{ value: "node", text: t("detail.directNode") }, { value: "label", text: t("source.label") }],
      render: (_, n) => n.source === "node" ? t("detail.directNode") : <span title={sourceText(t, n)}>{sourceText(t, n)}</span>,
    },
    {
      key: "time", title: t("detail.attachedTime"), width: 160,
      render: (_, node) => {
        const time = attachments.get(node.attachment_id)?.created_at;
        return time ? formatTime(time) : "—";
      },
    },
    {
      key: "actions", role: "actions", title: t("table.actions"), width: 124,
      render: (_, node) => {
        const attachment = attachments.get(node.attachment_id);
        if (!attachment) return "—";
        if (attachment.target === "label") return <Button variant="link" size="sm" onClick={() => setLabelEditor({ attachment })}>{t("nodeTable.editSourceLabel")}</Button>;
        return <span className="inline-flex items-center gap-2">
          <Button variant="link" size="sm" onClick={() => onAttach(attachment)}>{t("actions.edit")}</Button>
          <ConfirmPopover title={t("actions.confirmDetach")} onConfirm={() => detach(attachment)} confirmText={t("actions.detach")} cancelText={t("actions.cancel")} destructive trigger={<Button variant="link" size="sm" className="text-fg-danger">{t("actions.detach")}</Button>} />
        </span>;
      },
    },
  ];
  const expandedRowRender = (node: NetworkNode) => {
    const attachment = attachments.get(node.attachment_id);
    return <dl className="network-node-details ms-10 grid gap-3 border-s-2 border-border px-4 py-3 text-body">
      {rails ? (
        <>
          <div>
            <dt className="mb-1 text-label text-fg-muted">{t("nodeTable.bootstrap")}</dt>
            <dd><BootstrapCell network={detail} node={node} onChanged={onDetail} /></dd>
          </div>
          <div>
            <dt className="mb-1 text-label text-fg-muted">{t("nodeTable.hcaIPs")}<LinkLayerTag layer={node.link_layer} /></dt>
            <dd><RailsCell network={detail} node={node} onChanged={onDetail} /></dd>
          </div>
        </>
      ) : (
        <div>
          <dt className="mb-1 text-label text-fg-muted">{t("nodeTable.endpoint")}</dt>
          <dd><Endpoint iface={node.interface} ip={node.ip} /></dd>
        </div>
      )}
      {attachment && (attachment.interface || attachment.ip || attachment.hcas.length > 0) && (
        <div className="min-w-0 whitespace-normal break-all">
          <dt className="mb-1 text-label text-fg-muted">{t("attach.overrides")}</dt>
          <dd className="max-h-40 overflow-y-auto">
            {attachment.interface && <div>{t("attach.interface")} · {attachment.interface}</div>}
            {attachment.ip && <div>IP · {attachment.ip}</div>}
            {attachment.hcas.length > 0 && <div>HCA · {attachment.hcas.join(", ")}</div>}
          </dd>
        </div>
      )}
    </dl>;
  };

  return (
    <>
      {description && <Typography.Paragraph type="secondary">{description}</Typography.Paragraph>}
      <DetailSection title={t("detail.selector")}>
        {rails ? (
          <>
            <Descriptions bordered size="small" column={1}>
              <Descriptions.Item label={t("selector.bootstrapCidr")}>{detail.cidr || t("selector.inferredShort")}</Descriptions.Item>
            </Descriptions>
            <Typography.Paragraph type="secondary" style={{ fontSize: 12, margin: "8px 0 0" }} className="flex items-center gap-1">
              {t(isAcceleratorRDMA(detail.type) ? "detail.acceleratorEditHint" : "detail.rdmaEditHint")}
              <HelpTip content={t(isAcceleratorRDMA(detail.type) ? "detail.acceleratorInferredHelp" : "detail.rdmaInferredHelp")} />
            </Typography.Paragraph>
          </>
        ) : (
          <Descriptions bordered size="small" column={1}>
            <Descriptions.Item label={t("selector.cidr")}>{detail.cidr || "—"}</Descriptions.Item>
            <Descriptions.Item label={t("selector.interface")}>{detail.interface || "—"}</Descriptions.Item>
          </Descriptions>
        )}
      </DetailSection>
      <NetworkLoadTargets detail={detail} onAttach={onAttach} onDetach={detach} onManageLabels={() => setLabelEditor({})}>
        <div className="network-overview-nodes"><NetworkTargetTable<NetworkNode> networkId={detail.id} kind="nodes"
          toolbarExtra={<Button variant="secondary" size="sm" onClick={() => onAttach(null)}>{t("detail.attachNodes")}</Button>}
          tableId="network-resolved-nodes"
          rowKey="node_uuid"
          columns={nodeColumns}
          data={detail.nodes}
          emptyDescription={t("detail.noNodes")}
          expandable={{ expandedRowRender, expandRowByClick: true }}
        /></div>
      </NetworkLoadTargets>
      {labelEditor && <NetworkLabelsDialog open detail={detail} initialAttachment={labelEditor.attachment} onClose={() => setLabelEditor(null)} onDetail={onDetail} />}
    </>
  );
}

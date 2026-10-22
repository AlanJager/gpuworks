import { useCallback, useEffect, useState } from "react";
import { Alert, Button, Card, Col, Descriptions, Popconfirm, Row, Space, Table, Tabs, Tag, Typography, message } from "antd";
import type { ColumnsType } from "antd/es/table";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import type { NetworkAttachment, NetworkDetail, NetworkNode } from "../../networkTypes";
import PageHeader from "../../components/PageHeader";
import { formatTime } from "../../i18n/datetime";
import AttachNetworkModal from "./AttachNetworkModal";
import NetworkFormModal from "./NetworkFormModal";
import NetworkTestPanel from "./NetworkTestPanel";
import NetworkMonitorPanel from "./NetworkMonitorPanel";
import { Endpoint, RoleTags, TypeTag, WarningMark, isAcceleratorRDMA, usesRails, sourceText } from "./networkDisplay";
import { BootstrapCell, RailsCell } from "./RDMAEndpoints";
import ManagementNetworkDetail from "./ManagementNetworkDetail";

export default function NetworkDetailPage() {
  const { t } = useTranslation("networks");
  const navigate = useNavigate();
  const id = Number(useParams().id);
  const [detail, setDetail] = useState<NetworkDetail | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [attachOpen, setAttachOpen] = useState(false);
  const [editingAttachment, setEditingAttachment] = useState<NetworkAttachment | null>(null);
  const [formOpen, setFormOpen] = useState(false);

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

  if (error) return <Alert type="error" showIcon message={error.message} action={<Link to="/networks">{t("detail.back")}</Link>} />;
  if (!detail) return <Card loading />;
  if (detail.builtin) return <ManagementNetworkDetail detail={detail} onChanged={load} />;

  const detach = async (attachment: NetworkAttachment) => {
    try {
      setDetail(await api.detachNetwork(detail.id, attachment.id));
    } catch (e) {
      message.error(t("message.failed", { detail: (e as Error).message }));
    }
  };

  const nodeColumns: ColumnsType<NetworkNode> = [
    {
      title: t("nodeTable.node"),
      dataIndex: "node_name",
      render: (_, n) => <span>{n.node_name}<WarningMark warnings={n.warnings} /></span>,
    },
    { title: t("nodeTable.roles"), dataIndex: "roles", render: (_, n) => <RoleTags roles={n.roles} /> },
    {
      title: t("nodeTable.source"),
      dataIndex: "source",
      render: (_, n) => sourceText(t, n),
    },
    ...(usesRails(detail.type)
      ? [
        { title: t("nodeTable.bootstrap"), key: "bootstrap", render: (_: unknown, n: NetworkNode) => <BootstrapCell network={detail} node={n} onChanged={setDetail} /> },
        {
          title: <span>{t("nodeTable.hcaIPs")}{detail.nodes[0]?.link_layer && <Tag style={{ marginLeft: 6 }}>{t(`linkLayer.${detail.nodes[0].link_layer}`)}</Tag>}</span>,
          key: "hcas",
          render: (_: unknown, n: NetworkNode) => <RailsCell network={detail} node={n} onChanged={setDetail} />,
        },
      ]
      : [{ title: t("nodeTable.endpoint"), key: "endpoint", render: (_: unknown, n: NetworkNode) => <Endpoint iface={n.interface} ip={n.ip} /> }]),
  ];

  const overrideText = (a: NetworkAttachment) =>
    [a.interface, a.ip, a.hcas.join(",")].filter(Boolean).join(" · ");

  return (
    <>
      <PageHeader
        title={detail.name}
        description={detail.description}
        breadcrumb={[t("breadcrumb.resources"), t("title"), detail.name]}
        extra={
          <Space>
            <RoleTags roles={detail.roles} />
            <TypeTag type={detail.type} />
            <Button onClick={() => setFormOpen(true)}>{t("actions.edit")}</Button>
            <Popconfirm
              title={t("actions.confirmDelete", { name: detail.name })}
              onConfirm={async () => {
                try {
                  await api.deleteNetwork(detail.id);
                  message.success(t("message.deleted"));
                  navigate("/networks");
                } catch (e) {
                  message.error(t("message.failed", { detail: (e as Error).message }));
                }
              }}
              okText={t("actions.delete")}
              cancelText={t("actions.cancel")}
            >
              <Button danger>{t("actions.delete")}</Button>
            </Popconfirm>
          </Space>
        }
      />
      <Tabs
        items={[
          { key: "overview", label: t("tabs.overview"), children: (
            <>
            <Row gutter={16}>
              <Col xs={24} lg={10}>
                <Card title={t("detail.selector")} style={{ marginBottom: 16 }}>
                  {usesRails(detail.type) ? (
                    <Space direction="vertical" size={8}>
                      <Descriptions column={1} size="small">
                        <Descriptions.Item label={t("selector.bootstrapCidr")}>{detail.cidr || t("selector.inferredShort")}</Descriptions.Item>
                      </Descriptions>
                      <Typography.Text type="secondary">{t(isAcceleratorRDMA(detail.type) ? "detail.acceleratorInferred" : "detail.rdmaInferred")}</Typography.Text>
                    </Space>
                  ) : (
                    <Descriptions column={1} size="small">
                      <Descriptions.Item label={t("selector.cidr")}>{detail.cidr || "—"}</Descriptions.Item>
                      <Descriptions.Item label={t("selector.interface")}>{detail.interface || "—"}</Descriptions.Item>
                    </Descriptions>
                  )}
                </Card>
              </Col>
              <Col xs={24} lg={14}>
                <Card
                  title={t("detail.attachments")}
                  style={{ marginBottom: 16 }}
                  extra={<Button size="small" onClick={() => { setEditingAttachment(null); setAttachOpen(true); }}>{t("actions.attach")}</Button>}
                >
                  {detail.attachments.length === 0 ? (
                    <Typography.Text type="secondary">{t("detail.noAttachments")}</Typography.Text>
                  ) : (
                    <Table<NetworkAttachment>
                      rowKey="id"
                      size="small"
                      pagination={false}
                      showHeader={false}
                      dataSource={detail.attachments}
                      columns={[
                        {
                          key: "target",
                          render: (_, a) => (
                            <Space direction="vertical" size={0}>
                              <span>
                                <Tag>{t(`source.${a.target}`)}</Tag>
                                {a.target === "node" ? (a.node_name || a.node_uuid) : <Typography.Text code>{`${a.label_key}=${a.label_value ?? ""}`}</Typography.Text>}
                              </span>
                              {overrideText(a) && <Typography.Text type="secondary">{t("detail.overrides", { value: overrideText(a) })}</Typography.Text>}
                            </Space>
                          ),
                        },
                        { key: "time", render: (_, a) => <Typography.Text type="secondary">{t("detail.attachedAt", { time: formatTime(a.created_at) })}</Typography.Text> },
                        {
                          key: "actions",
                          align: "right",
                          render: (_, a) => (
                            <Space size={0}>
                              {a.target === "node" && (
                                <Button type="link" size="small" onClick={() => { setEditingAttachment(a); setAttachOpen(true); }}>{t("actions.edit")}</Button>
                              )}
                              <Popconfirm title={t("actions.confirmDetach")} onConfirm={() => detach(a)} okText={t("actions.detach")} cancelText={t("actions.cancel")}>
                                <Button type="link" size="small" danger>{t("actions.detach")}</Button>
                              </Popconfirm>
                            </Space>
                          ),
                        },
                      ]}
                    />
                  )}
                </Card>
              </Col>
            </Row>
            <Card title={t("detail.resolved", { count: detail.nodes.length })}>
              <Table<NetworkNode>
                rowKey="node_uuid"
                size="middle"
                pagination={{ defaultPageSize: 20, hideOnSinglePage: true, showSizeChanger: true, pageSizeOptions: [10, 20, 50, 100] }}
                columns={nodeColumns}
                dataSource={detail.nodes}
                locale={{ emptyText: t("detail.noNodes") }}
                scroll={{ x: true }}
              />
            </Card>
            </>
          ) },
          { key: "test", label: t("tabs.test"), children: <NetworkTestPanel network={detail} onStarted={load} onChanged={setDetail} /> },
          { key: "monitor", label: t("tabs.monitor"), children: <NetworkMonitorPanel network={detail} /> },
        ]}
      />
      <AttachNetworkModal
        open={attachOpen}
        network={detail}
        attachment={editingAttachment}
        onClose={() => setAttachOpen(false)}
        onSaved={(d) => { setAttachOpen(false); setDetail(d); }}
      />
      <NetworkFormModal
        open={formOpen}
        network={detail}
        roleOptions={detail.role_options}
        typeOptions={detail.type_options}
        typeRoles={detail.type_roles}
        onClose={() => setFormOpen(false)}
        onSaved={(d) => { setFormOpen(false); setDetail(d); }}
      />
    </>
  );
}

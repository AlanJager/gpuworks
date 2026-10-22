import { useEffect, useState } from "react";
import { Alert, Button, Card, Form, Input, InputNumber, Radio, Select, Space, Switch, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import type { NetworkDetail, NetworkTestEstimate, NetworkTestPattern, NetworkTestRequest } from "../../networkTypes";
import { isAcceleratorRDMA, isHostRDMA, usesRails } from "./networkDisplay";
import { isCardKind } from "./cardTest";
import NetworkTestResult from "./NetworkTestResult";

/** Above this many nodes a test defaults to a ring: a mesh grows as N². */
export const ringDefaultAbove = 16;

interface FormValues {
  kind: NonNullable<NetworkTestRequest["kind"]>;
  pattern: NetworkTestPattern;
  scope: NetworkTestRequest["scope"];
  node_uuids?: string[];
  label_key?: string;
  label_value?: string;
  bandwidth: boolean;
  seconds: number;
}

/** Seconds as the viewer reads a duration: seconds below a minute, minutes above. */
function duration(t: (key: string, options?: Record<string, unknown>) => string, seconds: number) {
  return seconds < 60 ? t("test.durationSeconds", { count: seconds }) : t("test.durationMinutes", { value: (seconds / 60).toFixed(1).replace(/\.0$/, "") });
}

/** The request a form's values stand for. Ethernet networks have no kind choice; they only run TCP tests. */
function testBody(v: FormValues): NetworkTestRequest {
  const kind = v.kind ?? "tcp";
  const body: NetworkTestRequest = isCardKind(kind)
    ? { kind, pattern: v.pattern, scope: v.scope }
    : { kind, pattern: v.pattern, scope: v.scope, bandwidth: kind === "tcp" ? v.bandwidth : true, seconds: v.seconds };
  if (v.scope === "nodes") body.node_uuids = v.node_uuids;
  if (v.scope === "label") { body.label_key = v.label_key?.trim(); body.label_value = v.label_value?.trim() ?? ""; }
  return body;
}

/** Starts a network's internal test and shows the latest result. */
export default function NetworkTestPanel({ network, onStarted, onChanged }: { network: NetworkDetail; onStarted: () => void; onChanged: (detail: NetworkDetail) => void }) {
  const { t } = useTranslation("networks");
  const [form] = Form.useForm<FormValues>();
  const scope = Form.useWatch("scope", form);
  const kind = (Form.useWatch("kind", form) ?? "tcp") as FormValues["kind"];
  const pattern = (Form.useWatch("pattern", form) ?? "mesh") as NetworkTestPattern;
  // Nodes that cannot run ib_write_bw, named before the test starts.
  const kindOptions = [
    { value: "tcp", label: t("test.kindTcp") },
    ...(isHostRDMA(network.type) ? [{ value: "rdma_tool", label: t("test.kindRdmaTool") }] : []),
    ...(isAcceleratorRDMA(network.type) ? [{ value: "card_ping", label: t("test.kindCardPing") }, { value: "card_matrix", label: t("test.kindCardMatrix") }] : []),
  ];
  const card = isCardKind(kind);
  const chosen = Form.useWatch("node_uuids", form);
  const labelKey = Form.useWatch("label_key", form);
  const labelValue = Form.useWatch("label_value", form);
  // How large a card test is, and whether it may start, is the server's to
  // say: it resolves the scope to nodes and lays the rounds out as the run
  // will. Each change of the form asks again.
  const [estimate, setEstimate] = useState<{ value?: NetworkTestEstimate; error?: string }>({});
  const chosenKey = (chosen ?? []).join(",");
  useEffect(() => {
    setEstimate({});
    if (!card || !scope) return;
    if (scope === "nodes" && (chosen ?? []).length < 2) return;
    if (scope === "label" && !labelKey?.trim()) return;
    let stale = false;
    const timer = setTimeout(() => {
      api.estimateNetworkTest(network.id, testBody({ kind, pattern, scope, node_uuids: chosen, label_key: labelKey, label_value: labelValue, bandwidth: false, seconds: 0 }))
        .then((value) => { if (!stale) setEstimate({ value }); })
        .catch((e: Error) => { if (!stale) setEstimate({ error: e.message }); });
    }, 200);
    return () => { stale = true; clearTimeout(timer); };
    // chosenKey stands for the chosen nodes: the array is new on every render.
  }, [card, kind, pattern, scope, chosenKey, labelKey, labelValue, network.id]);
  const missingTool = network.nodes.filter((n) => !n.tools?.includes("ib_write_bw")).map((n) => n.node_name);
  // Nodes whose pairs fail before any traffic: said before the test starts.
  const noAddress = network.nodes.filter((n) => !n.ip).map((n) => n.node_name);
  const noRails = usesRails(network.type) ? network.nodes.filter((n) => n.hcas.length === 0).map((n) => n.node_name) : [];
  const [starting, setStarting] = useState(false);
  const run = network.latest_test;
  const running = run?.status === "running";

  if (!network.test_supported) return <Alert type="info" showIcon message={t("test.unsupported")} />;

  const start = async () => {
    let v: FormValues;
    try {
      v = await form.validateFields();
    } catch {
      return;
    }
    const body = testBody(v);
    setStarting(true);
    try {
      await api.startNetworkTest(network.id, body);
      message.success(t("test.started"));
      onStarted();
    } catch (e) {
      message.error(t("message.failed", { detail: (e as Error).message }));
    } finally {
      setStarting(false);
    }
  };

  return (
    <Space direction="vertical" size={16} style={{ width: "100%" }}>
      <Card title={t("test.start")}>
        <Typography.Paragraph type="secondary">{t("test.what")}</Typography.Paragraph>
        <Form form={form} layout="vertical" initialValues={{ kind: isAcceleratorRDMA(network.type) ? "card_ping" : "tcp", pattern: network.nodes.length > ringDefaultAbove ? "ring" : "mesh", scope: "all", bandwidth: true, seconds: 3, node_uuids: [] }}>
          {kindOptions.length > 1 && (
            <Form.Item label={t("test.kind")} name="kind">
              <Radio.Group optionType="button" options={kindOptions} />
            </Form.Item>
          )}
          {noAddress.length > 0 && <Alert style={{ marginBottom: 16 }} type="warning" showIcon message={t(usesRails(network.type) ? "test.noBootstrap" : "test.noAddress", { nodes: noAddress.join(", ") })} />}
          {card && <Alert style={{ marginBottom: 16 }} type="info" showIcon message={t(`test.cardNote.${kind}`)} />}
          {(kind === "rdma_tool" || card) && noRails.length > 0 && <Alert style={{ marginBottom: 16 }} type="warning" showIcon message={t("test.noRails", { nodes: noRails.join(", ") })} />}
          {usesRails(network.type) && kind === "tcp" && <Alert style={{ marginBottom: 16 }} type="info" showIcon message={t(isHostRDMA(network.type) ? "test.rdmaNote" : "test.acceleratorNote")} />}
          {kind === "rdma_tool" && (
            <Alert
              style={{ marginBottom: 16 }}
              type={missingTool.length ? "warning" : "info"}
              showIcon
              message={missingTool.length ? t("test.toolMissing", { nodes: missingTool.join(", ") }) : t("test.toolReady")}
            />
          )}
          <Form.Item label={t("test.pattern")} name="pattern" extra={t(card ? `test.cardPatternHint.${pattern}` : `test.patternHint.${pattern}`)}>
            <Radio.Group optionType="button" options={[
              { value: "mesh", label: t("test.patternName.mesh") },
              { value: "ring", label: t("test.patternName.ring") },
            ]} />
          </Form.Item>
          <Form.Item label={t("test.scope")} name="scope">
            <Radio.Group optionType="button" options={[
              { value: "all", label: t("test.scopeAll") },
              { value: "nodes", label: t("test.scopeNodes") },
              { value: "label", label: t("test.scopeLabel") },
            ]} />
          </Form.Item>
          {scope === "nodes" && (
            <Form.Item label={t("test.nodes")} name="node_uuids" rules={[{ type: "array", min: 2, message: t("test.nodesRequired") }]}>
              <Select mode="multiple" optionFilterProp="label" options={network.nodes.map((n) => ({ value: n.node_uuid, label: n.node_name }))} />
            </Form.Item>
          )}
          {scope === "label" && (
            <Space wrap>
              <Form.Item label={t("test.labelKey")} name="label_key" rules={[{ required: true, whitespace: true }]}><Input placeholder="rack" /></Form.Item>
              <Form.Item label={t("test.labelValue")} name="label_value"><Input placeholder="r3" /></Form.Item>
            </Space>
          )}
          {card ? (
            <Alert
              style={{ marginBottom: 16 }}
              type={estimate.error ? "error" : "info"}
              showIcon
              message={estimate.error
                ? t("test.cardEstimateFailed", { detail: estimate.error })
                : estimate.value?.card
                  ? t("test.cardEstimate", { nodes: estimate.value.node_count, pairs: estimate.value.pair_count, probes: estimate.value.card.probes, rounds: estimate.value.card.rounds, duration: duration(t, estimate.value.card.seconds) })
                  : t("test.cardEstimating")}
            />
          ) : (
            <Space wrap size={24}>
              {kind === "tcp" && <Form.Item label={t("test.bandwidth")} name="bandwidth" valuePropName="checked"><Switch /></Form.Item>}
              <Form.Item label={t("test.seconds")} name="seconds"><InputNumber min={1} max={10} /></Form.Item>
            </Space>
          )}
          <div><Button type="primary" onClick={start} loading={starting} disabled={running || (card && !!estimate.error)}>{t("test.start")}</Button></div>
        </Form>
      </Card>
      <NetworkTestResult network={network} run={run} onChanged={onChanged} />
    </Space>
  );
}

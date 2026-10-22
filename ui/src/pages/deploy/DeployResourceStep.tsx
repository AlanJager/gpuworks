import {
  Alert,
  Col,
  Form,
  Input,
  InputNumber,
  Row,
  Segmented,
  Select,
  Space,
  Switch,
  Tag,
  Typography,
} from "antd";
import type {
  EligibleNode,
  EvaluateResponse,
  Node,
  PartitionCapability as Capability,
  PreflightSummary,
} from "../../api";
import { useTranslation } from "react-i18next";
import { nodeDisplayName } from "../../nodeDisplay";
import PartitionCapability from "../../components/PartitionCapability";
import NodePlacementLabels from "../../components/NodePlacementLabels";
import EligibleNodePicker from "./EligibleNodePicker";
import DeployParallelFields from "./DeployParallelFields";
import { gpuSelectOptions } from "./gpuOptions";

const ONLINE = { state: ["online"] };

// The node choices search online nodes on the server and render its
// eligibility answer (EligibleNodePicker): ineligible ones carry their
// reasons. Before the first evaluation every online node is optionable.
export default function DeployResourceStep({
  eligibleNodes,
  selectedNode,
  capability,
  preflight,
  result,
}: {
  eligibleNodes?: EligibleNode[];
  selectedNode?: Node;
  capability?: Capability;
  preflight?: PreflightSummary;
  result: EvaluateResponse | null;
}) {
  const { t } = useTranslation("deploy");
  const { t: tc } = useTranslation("common");
  const form = Form.useFormInstance();
  const watchOptions = { form, preserve: true } as const;
  const nodeMode =
    Form.useWatch("node_selection_mode", watchOptions) || "manual";
  const nnodes = Number(Form.useWatch("nnodes", watchOptions) || 1);
  const vramLimit = Number(Form.useWatch("vram_limit_gib", watchOptions) || 0);
  const supportsPartition = capability?.status === "ready";
  return (
    <div>
      <Typography.Title level={4}>{t("resource.title")}</Typography.Title>
      <NodePlacementLabels scope={ONLINE} />
      <Row gutter={16}>
        <Col xs={24} md={12}>
          <Form.Item
            label={t("resource.targetNode")}
            name="node_selection_mode"
          >
            <Segmented
              block
              options={[
                { value: "auto", label: t("resource.autoSelect") },
                { value: "manual", label: t("resource.manualSelect") },
              ]}
            />
          </Form.Item>
        </Col>
        <Col xs={24} md={12}>
          {nodeMode === "manual" ? (
            <Form.Item
              label={t("resource.onlineNode")}
              name="node_uuid"
              rules={[{ required: true, message: t("resource.nodeRequired") }]}
            >
              <EligibleNodePicker eligibleNodes={eligibleNodes} />
            </Form.Item>
          ) : (
            <Form.Item label={t("resource.autoResult")}>
              <Input
                readOnly
                value={
                  selectedNode
                    ? `${nodeDisplayName(selectedNode)} · ${selectedNode.gpu_count} GPU`
                    : t("resource.noNode")
                }
              />
            </Form.Item>
          )}
        </Col>
        <Col xs={24} md={12}>
          <Form.Item
            label={t("resource.gpuSelection")}
            name="gpu_indexes"
            extra={t("resource.gpuSelectionHelp")}
          >
            <Select
              mode="multiple"
              allowClear
              placeholder={t("resource.autoSelect")}
              options={gpuSelectOptions(selectedNode?.gpus, tc)}
            />
          </Form.Item>
        </Col>
        <DeployParallelFields
          result={result}
          multi={nnodes > 1}
          tpLabel={t("resource.tensorParallel")}
          ppLabel={t("resource.pipelineParallel")}
          wrap={(field, key) => (
            <Col xs={24} md={12} key={key}>
              {field}
            </Col>
          )}
        />
        <Col xs={24} md={12}>
          <Form.Item label={t("resource.nodeCount")} name="nnodes">
            <InputNumber style={{ width: "100%" }} min={1} max={16} />
          </Form.Item>
        </Col>
        <Col xs={24} md={12}>
          <Form.Item
            label={t("catalog.replicas")}
            name="replicas"
            extra={t("catalog.replicasHelp")}
            rules={[
              {
                validator: (_, value) =>
                  nnodes > 1 && Number(value || 1) !== 1
                    ? Promise.reject(
                        new Error(t("catalog.replicasDistributed")),
                      )
                    : Promise.resolve(),
              },
            ]}
          >
            <InputNumber
              style={{ width: "100%" }}
              min={1}
              max={nnodes > 1 ? 1 : 32}
            />
          </Form.Item>
        </Col>
        <Col xs={24} md={12}>
          <Form.Item
            label={t("resource.sleepable")}
            name="sleepable"
            valuePropName="checked"
          >
            <Switch />
          </Form.Item>
        </Col>
        <Form.Item name="partition_backend" hidden>
          <Input />
        </Form.Item>
        <Col span={24}>
          <PartitionCapability capability={capability} />
        </Col>
        {supportsPartition ? (
          <Col xs={24} md={12}>
            <Form.Item
              label={t("resource.partition")}
              name="vram_limit_gib"
              extra={t("resource.partitionHelp")}
            >
              <InputNumber
                style={{ width: "100%" }}
                min={1}
                placeholder={t("resource.fullGpu")}
              />
            </Form.Item>
          </Col>
        ) : (
          <Col xs={24} md={12}>
            <Alert
              type="info"
              showIcon
              message={
                nodeMode === "auto"
                  ? t("resource.dgpuUnavailablePool")
                  : t("resource.dgpuUnavailable")
              }
            />
          </Col>
        )}
        {supportsPartition &&
          capability?.maturity === "experimental" &&
          vramLimit > 0 && (
            <Col span={24}>
              <Alert
                type="warning"
                showIcon
                message={t("resource.experimentalBackendTitle")}
                description={t("resource.experimentalBackendDescription")}
              />
            </Col>
          )}
        {nnodes > 1 && (
          <>
            <Col span={24}>
              <Form.Item
                label={t("resource.distributedNodes")}
                name="distributed_nodes"
                rules={[
                  {
                    required: true,
                    message: t("resource.distributedNodesRequired", {
                      count: nnodes,
                    }),
                  },
                ]}
              >
                <EligibleNodePicker
                  mode="multiple"
                  maxCount={nnodes}
                  eligibleNodes={eligibleNodes}
                  rankAddress={{ suffix: t("resource.addressMissing") }}
                />
              </Form.Item>
            </Col>
            <Col xs={24} md={8}>
              <Form.Item label={t("resource.masterAddress")} name="master_addr">
                <Input placeholder={t("resource.masterAddressPlaceholder")} />
              </Form.Item>
            </Col>
            <Col xs={24} md={8}>
              <Form.Item label={t("resource.masterPort")} name="master_port">
                <InputNumber style={{ width: "100%" }} min={1024} max={65535} />
              </Form.Item>
            </Col>
            <Col xs={24} md={8}>
              <Form.Item label={t("resource.group")} name="dist_group">
                <Input placeholder="qwen-prod" />
              </Form.Item>
            </Col>
            <Col span={24}>
              {preflight && (
                <Space wrap>
                  <Tag color={preflight.ok ? "green" : "red"}>
                    {t(
                      preflight.ok
                        ? "resource.preflightPassed"
                        : "resource.preflightFailed",
                    )}
                  </Tag>
                  {(preflight.checks ?? []).map((check, index) => (
                    <Tag key={index} color="red">
                      {check.message || String(check.code ?? "")}
                    </Tag>
                  ))}
                </Space>
              )}
            </Col>
          </>
        )}
      </Row>
    </div>
  );
}

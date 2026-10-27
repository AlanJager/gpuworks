import { useMemo } from "react";
import { Alert, Collapse, Form, InputNumber, Radio, Segmented, Space, Tag, Typography } from "antd";
import type { EligibleNode, EngineProfile, EvaluateResponse, Node, NodeFacets } from "../../api";
import NodePlacementLabels from "../../components/NodePlacementLabels";
import SystemLabelConditions from "../../components/SystemLabelConditions";
import { findTemplateSpec } from "./deployHelpers";
import { ChoiceCard, GPUGrid } from "./DeployChoiceCards";
import DeployPdSection from "./DeployPdSection";
import { pdModesForEngine, pdModesForVendor } from "./deployPd";
import { useTranslation } from "react-i18next";
import { nodeDisplayName } from "../../nodeDisplay";
import NodePicker from "../../nodes/NodePicker";
import { gpuModelSummary } from "../../nodes/cardIdentity";
import EligibleNodePicker from "./EligibleNodePicker";
import { nodeRefusals, nodeConditionSummary } from "./deployPlacementRefusal";
import DeployNodeRefusals from "./DeployNodeRefusals";

const ONLINE = { state: ["online"] };

// The pickers search online nodes on the server and render its eligibility
// answer (EligibleNodePicker). fleet is what the online nodes carry (GET
// /nodes/facets); knownNodes are the nodes the draft names, read one by one.
export default function NodeSection({
  fleet,
  knownNodes,
  eligibleNodes,
  selectedNode,
  modelReady,
  result,
  loading,
  profile,
  onNodeMode,
}: {
  fleet?: NodeFacets | null;
  knownNodes: Map<string, Node>;
  eligibleNodes?: EligibleNode[];
  selection: ReturnType<typeof findTemplateSpec>;
  selectedNode?: Node;
  modelReady: boolean;
  result: EvaluateResponse | null;
  loading: boolean;
  profile?: EngineProfile;
  onNodeMode: (mode: "auto" | "manual" | "multi" | "pd") => void;
}) {
  const { t } = useTranslation("deploy");
  const form = Form.useFormInstance();
  const mode = (Form.useWatch("node_selection_mode", {
    form,
    preserve: true,
  }) || "auto") as "auto" | "manual" | "multi" | "pd";
  const single = mode === "auto" || mode === "manual";
  const nnodes = Number(Form.useWatch("nnodes", { form, preserve: true }) || 1);
  const distributed = (Form.useWatch("distributed_nodes", {
    form,
    preserve: true,
  }) || []) as string[];
  const targetVendor = Form.useWatch("target_vendor", {
    form,
    preserve: true,
  }) as string | undefined;
  const pdEngine = pdModesForEngine(profile).length > 0;
  const pdSupported = pdModesForVendor(profile, targetVendor).length > 0;
  // The auto card renders the server's selection; the manual dropdown reads
  // the form's own node field. Switching to manual starts from an empty pick,
  // never from the placement the server made for the auto draft.
  const autoSelected = result?.selected_node
    ? knownNodes.get(result.selected_node.uuid)
    : undefined;
  const ready = modelReady && Boolean(targetVendor);
  // Nodes the scheduler judged and turned down: with them, "nothing
  // matches" would hide the reasons the deployer can act on.
  const refusals = autoSelected ? [] : nodeRefusals(t, result);
  const conditions = !loading ? nodeConditionSummary(result) : null;
  const vendorPool = useMemo(
    () => ({ ...ONLINE, label: targetVendor ? [`gpuworks.ai/vendor=${targetVendor}`] : [] }),
    [targetVendor],
  );
  return (
    <div className="deploy-workbench-row">
      <div className="deploy-workbench-label">{t("workbench.node.title")}</div>
      <div className="deploy-workbench-body">
        <NodePlacementLabels scope={ONLINE} />
        <SystemLabelConditions facets={fleet?.system_labels} />
        <div className="deploy-choice-strip">
          <ChoiceCard
            active={single}
            disabled={!ready}
            title={t("workbench.node.single")}
            description={t("workbench.node.singleDescription")}
            onClick={() => onNodeMode(single ? mode : "auto")}
          />
          <ChoiceCard
            active={mode === "multi"}
            disabled={!ready}
            title={t("workbench.node.multi")}
            description={t("workbench.node.count", {
              count: Math.max(2, nnodes),
            })}
            onClick={() => onNodeMode("multi")}
          />
          <ChoiceCard
            active={mode === "pd"}
            disabled={!ready || !pdSupported}
            title={t("workbench.node.pd")}
            description={
              pdSupported
                ? t("workbench.node.pdDescription")
                : pdEngine
                  ? t("workbench.node.pdVendorUnsupported", {
                      vendor: t(`basic.vendor.${targetVendor}`, {
                        defaultValue: targetVendor?.toUpperCase(),
                      }),
                    })
                  : t("workbench.node.pdEngineUnsupported")
            }
            onClick={() => onNodeMode("pd")}
          />
        </div>
        {single && (
          <div className="deploy-node-pool">
            <Segmented
              disabled={!ready}
              value={mode}
              onChange={(value) => onNodeMode(value as "auto" | "manual")}
              options={[
                { value: "auto", label: t("workbench.node.auto") },
                { value: "manual", label: t("workbench.node.manual") },
              ]}
            />
            <Typography.Paragraph type="secondary" style={{ margin: "6px 0 0" }}>
              {mode === "auto" ? t("workbench.node.autoPoolDescription") : t("workbench.node.manualPoolDescription")}
            </Typography.Paragraph>
            {eligibleNodes && !loading && (
              <Typography.Paragraph type="secondary" style={{ margin: 0 }}>
                {conditions
                  ? t("workbench.node.conditionFunnel", { total: conditions.total, matched: conditions.matched.length, eligible: conditions.eligible })
                  : t("workbench.node.poolMatches", { count: eligibleNodes.filter((node) => node.eligible).length, total: fleet?.total ?? 0 })}
              </Typography.Paragraph>
            )}
            {mode === "auto" && (
              <Form.Item name="pool_nodes" label={t("workbench.node.poolNodes")} extra={t("workbench.node.poolNodesHelp")} style={{ margin: "8px 0 0" }}>
                <NodePicker
                  mode="multiple"
                  allowClear
                  disabled={!ready}
                  filter={vendorPool}
                  placeholder={t("workbench.node.poolNodesPlaceholder")}
                  aria-label={t("workbench.node.poolNodes")}
                />
              </Form.Item>
            )}
            {mode === "auto" && (
              <Collapse
                ghost
                size="small"
                items={[{
                  key: "placement",
                  label: t("workbench.node.placementOptions"),
                  forceRender: true,
                  children: (
                    <Form.Item name="placement_strategy" label={t("workbench.node.strategy")} initialValue="spread">
                      <Radio.Group>
                        <Radio value="spread">
                          {t("workbench.node.strategySpread")}
                          <Typography.Text type="secondary"> — {t("workbench.node.strategySpreadHelp")}</Typography.Text>
                        </Radio>
                        <Radio value="binpack">
                          {t("workbench.node.strategyBinpack")}
                          <Typography.Text type="secondary"> — {t("workbench.node.strategyBinpackHelp")}</Typography.Text>
                        </Radio>
                      </Radio.Group>
                    </Form.Item>
                  ),
                }]}
              />
            )}
          </div>
        )}
        {mode === "auto" && (
          <div className="deploy-node-card is-selected">
            <div className="deploy-node-card-head">
              <strong>
                {autoSelected
                  ? t("workbench.node.schedulerSelected")
                  : t("workbench.node.schedulerWaiting")}
              </strong>
              <span>{t("workbench.node.schedulerDescription")}</span>
            </div>
            {autoSelected ? (
              <div className="deploy-node-item">
                <strong>
                  {nodeDisplayName(autoSelected)}
                </strong>
                <span>
                  {gpuModelSummary(autoSelected)}
                </span>
                <i />
                <GPUGrid
                  node={autoSelected}
                  automatic={result?.plan?.plan?.gpus}
                />
              </div>
            ) : (
              ready && !loading && refusals.length > 0 ? (
                <DeployNodeRefusals refusals={refusals} conditions={conditions} />
              ) : (
                <Alert
                  type="info"
                  showIcon
                  message={loading ? t("workbench.node.evaluating") : ready ? t("workbench.node.noCompatibleOnline") : t("workbench.node.chooseModelFirst")}
                />
              )
            )}
          </div>
        )}
        {mode === "manual" && (
          <div className="deploy-node-card">
            <div className="deploy-node-card-head">
              <strong>{t("workbench.node.selectNodeAndGpu")}</strong>
              <span>{t("workbench.node.incompatibleDisabled")}</span>
            </div>
            <Form.Item
              name="node_uuid"
              rules={[
                { required: true, message: t("workbench.node.required") },
              ]}
            >
              <EligibleNodePicker
                disabled={!ready}
                eligibleNodes={eligibleNodes}
                placeholder={t("workbench.node.placeholder")}
              />
            </Form.Item>
            {selectedNode && (
              <div className="deploy-node-item">
                <strong>
                  {nodeDisplayName(selectedNode)}
                </strong>
                <span>
                  {gpuModelSummary(selectedNode)}
                </span>
                <i />
                <GPUGrid node={selectedNode} />
              </div>
            )}
          </div>
        )}
        {mode === "multi" && (
          <div className="deploy-node-card">
            <div className="deploy-node-card-head">
              <strong>{t("workbench.node.selectMulti")}</strong>
              <Space size={8}>
                <span>{t("workbench.node.nodeCount")}</span>
                <Form.Item name="nnodes" noStyle>
                  <InputNumber min={2} max={16} size="small" aria-label={t("workbench.node.nodeCount")} />
                </Form.Item>
              </Space>
            </div>
            <Form.Item
              name="distributed_nodes"
              rules={[
                {
                  required: true,
                  message: t("workbench.node.multiRequired", {
                    count: Math.max(2, nnodes),
                  }),
                },
              ]}
            >
              <EligibleNodePicker
                disabled={!ready}
                mode="multiple"
                maxCount={Math.max(2, nnodes)}
                eligibleNodes={eligibleNodes}
                rankAddress={{ suffix: t("workbench.node.noRankAddress"), hint: t("workbench.node.noRankAddressHint") }}
                placeholder={t("workbench.node.multiPlaceholder")}
              />
            </Form.Item>
            <div className="deploy-multi-node-list">
              {distributed.map((uuid, index) => {
                const node = knownNodes.get(uuid);
                return (
                  <div className="deploy-node-item" key={uuid}>
                    <Tag color={index === 0 ? "blue" : "default"}>
                      {index === 0 ? "Master" : "Worker"}
                    </Tag>
                    <strong>{node ? nodeDisplayName(node) : uuid.slice(0, 8)}</strong>
                    <span>{node?.gpu_count || 0} GPU</span>
                  </div>
                );
              })}
            </div>
            <Space wrap>
              {result?.preflight && (
                <Tag color={result.preflight.ok ? "success" : "error"}>
                  {result.preflight.ok
                    ? t("workbench.node.preflightPassed")
                    : t("workbench.node.preflightFailed")}
                </Tag>
              )}
            </Space>
          </div>
        )}
        {mode === "pd" && (
          <DeployPdSection
            eligibleNodes={eligibleNodes}
            result={result}
            loading={loading}
            profile={profile}
          />
        )}
      </div>
    </div>
  );
}

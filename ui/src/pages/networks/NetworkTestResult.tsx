import { useMemo, useState } from "react";
import { Alert, Button, Card, Descriptions, Modal, Space, Table, Tag, Tooltip, Typography } from "antd";
import type { ColumnsType } from "antd/es/table";
import { useTranslation } from "react-i18next";
import type { NetworkDetail, NetworkTestPair, NetworkTestRail, NetworkTestRun, NetworkTestStatus } from "../../networkTypes";
import { formatTime } from "../../i18n/datetime";
import { RemoveHCAButton } from "./RDMAEndpoints";
import { testScopeText, usesRails } from "./networkDisplay";
import CardTestResult from "./CardTestResult";
import { isCardKind } from "./cardTest";
import { failureText as failureTextOf } from "./testFailure";

type T = (key: string, options?: Record<string, unknown>) => string;

const statusColor: Record<NetworkTestStatus, string> = { passed: "green", partial: "orange", failed: "red" };

/** Codes the client knows how to say; anything else shows the server's text. */
const knownCodes = new Set([
  "offline", "deleted", "no_address", "no_hca", "tool_missing", "no_roce_v2_gid", "tool_failed",
  "no_peer_rail", "listener_failed", "rpc_failed", "probe_failed", "bandwidth_failed",
]);

/** A failure in the viewer's language: the code with the node it is about and the tool's own detail. */
export function failureText(t: T, f: { code?: string; side?: string; error?: string }, node?: string) {
  return failureTextOf(t, "test.error", knownCodes, f, node);
}

/** Which node a failed rail is about, and the rail's name on that node. */
function railAtFault(pair: NetworkTestPair, rail: NetworkTestRail) {
  if (rail.side === "target" || !rail.rail) return { uuid: pair.target_uuid, name: pair.target_name, hca: rail.target_rail ?? "" };
  return { uuid: pair.source_uuid, name: pair.source_name, hca: rail.rail };
}

function railOK(r: NetworkTestRail) {
  return !r.code && !r.error && (r.gbps > 0 || !!r.reachable);
}

interface Problem {
  key: string;
  node: string;
  nodeUUID: string;
  hca?: string;
  text: string;
  pairs: number;
}

/**
 * Failures grouped by what caused them: a rail on one node, or a whole node
 * (offline, no tool, no address). This is what to fix first.
 */
function problems(t: T, run: NetworkTestRun): Problem[] {
  const out = new Map<string, Problem>();
  const add = (p: Omit<Problem, "pairs">) => {
    const seen = out.get(p.key);
    if (seen) seen.pairs++;
    else out.set(p.key, { ...p, pairs: 1 });
  };
  for (const pair of run.pairs) {
    if (pair.status === "passed") continue;
    if (pair.code || (pair.error && !pair.rails?.length)) {
      const onTarget = pair.side === "target";
      const node = onTarget ? pair.target_name : pair.side === "source" ? pair.source_name : `${pair.source_name} → ${pair.target_name}`;
      const uuid = onTarget ? pair.target_uuid : pair.source_uuid;
      add({ key: `pair|${pair.code ?? pair.error}|${node}`, node, nodeUUID: uuid, text: failureText(t, pair, node) });
      continue;
    }
    for (const rail of pair.rails ?? []) {
      if (railOK(rail)) continue;
      const at = railAtFault(pair, rail);
      add({ key: `rail|${at.uuid}|${at.hca}|${rail.code ?? rail.error}`, node: at.name, nodeUUID: at.uuid, hca: at.hca, text: failureText(t, rail, at.name) });
    }
  }
  return [...out.values()];
}

/** The latest run of a network: what failed, then the pairs. */
export default function NetworkTestResult({ network, run, onChanged }: {
  network: NetworkDetail;
  run: NetworkTestRun | null;
  onChanged: (detail: NetworkDetail) => void;
}) {
  const { t } = useTranslation("networks");
  const [pair, setPair] = useState<NetworkTestPair | null>(null);
  const [open, setOpen] = useState(false);
  const openPair = (selected: NetworkTestPair) => { setPair(selected); setOpen(true); };
  const found = useMemo(() => (run ? problems(t, run) : []), [t, run]);
  // Pairs far below the fastest one usually cross a slower link; flag them so
  // a mixed fabric is visible at a glance.
  const fastest = useMemo(() => Math.max(0, ...(run?.pairs ?? []).map((p) => p.gbps)), [run]);
  if (!run) return <Card><Typography.Text type="secondary">{t("test.none")}</Typography.Text></Card>;
  if (run.status === "running") return <Card><Typography.Text>{t("test.running", { nodes: run.node_count, pairs: run.pair_count })}</Typography.Text></Card>;
  if (run.status === "interrupted") return <Alert type="warning" showIcon message={t("test.interrupted", { time: formatTime(run.started_at) })} />;

  if (isCardKind(run.kind)) return <CardTestResult run={run} />;

  const rdma = run.kind === "rdma_tool";
  const failed = run.pairs.length - run.passed_count - run.partial_count;
  const summary = [
    t("test.summary", { time: formatTime(run.started_at), scope: testScopeText(t, run) }),
    t(`test.patternName.${run.pattern}`),
    t("test.pairCounts", { passed: run.passed_count, partial: run.partial_count, failed }),
    ...(rdma && run.rail_count > 0 ? [t("test.railCounts", { passed: run.rail_passed_count, total: run.rail_count })] : []),
  ].join(" · ");
  const slow = (p: NetworkTestPair) => run.bandwidth && p.status === "passed" && fastest > 0 && p.gbps < fastest / 10;

  return (
    <Space direction="vertical" size={16} style={{ width: "100%" }}>
      <Card title={`${t(`test.kindName.${run.kind}`)} · ${summary}`}>
        {found.length > 0 && (
          <div style={{ marginBottom: 16 }}>
            <Typography.Title level={5}>{t("test.problems")}</Typography.Title>
            <Table<Problem>
              rowKey="key"
              size="small"
              pagination={false}
              dataSource={found}
              columns={[
                { title: t("test.problemWhere"), key: "where", render: (_, p) => <span>{p.node}{p.hca && <Typography.Text code style={{ marginLeft: 6 }}>{p.hca}</Typography.Text>}</span> },
                { title: t("test.problemPairs"), dataIndex: "pairs", render: (v: number) => t("test.pairsAffected", { count: v }) },
                { title: t("test.problemWhy"), dataIndex: "text" },
                {
                  title: t("table.actions"),
                  key: "actions",
                  render: (_, p) => (p.hca && usesRails(network.type)
                    ? <RemoveHCAButton network={network} hca={p.hca} nodeUUID={p.nodeUUID} nodeName={p.node} onChanged={onChanged} label={t("rail.removeShort")} />
                    : null),
                },
              ]}
            />
          </div>
        )}
        {run.pattern === "ring"
          ? <PairList run={run} onOpen={openPair} slow={slow} />
          : <PairMatrix run={run} onOpen={openPair} slow={slow} />}
      </Card>
      <PairDetail network={network} run={run} pair={pair} open={open} onClose={() => setOpen(false)}
        afterClose={() => setPair(null)} onChanged={(d) => { setOpen(false); onChanged(d); }} />
    </Space>
  );
}

function PairTag({ pair, bandwidth, slow, onOpen }: { pair: NetworkTestPair; bandwidth: boolean; slow: boolean; onOpen: () => void }) {
  const { t } = useTranslation("networks");
  const rails = pair.rails ?? [];
  const okRails = rails.filter(railOK).length;
  let label = t("test.failed");
  if (pair.status !== "failed") label = bandwidth ? t("test.gbps", { value: pair.gbps.toFixed(1) }) : t("test.reachable");
  if (pair.status === "partial") label += ` · ${okRails}/${rails.length}`;
  const color = pair.status === "passed" && slow ? "orange" : statusColor[pair.status];
  const hint = pair.status === "passed" && slow ? t("test.slow") : failureText(t, pair) || (pair.status === "partial" ? t("test.partialHint") : "");
  return (
    <Tooltip trigger={["hover", "focus"]} title={hint || undefined}>
      <Tag
        role="button"
        tabIndex={0}
        color={color}
        style={{ cursor: "pointer" }}
        aria-label={`${pair.source_name} → ${pair.target_name}: ${label}`}
        onClick={onOpen}
        onKeyDown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); onOpen(); } }}
      >
        {label}
      </Tag>
    </Tooltip>
  );
}

function PairMatrix({ run, onOpen, slow }: { run: NetworkTestRun; onOpen: (p: NetworkTestPair) => void; slow: (p: NetworkTestPair) => boolean }) {
  const { t } = useTranslation("networks");
  const nodes = useMemo(() => {
    const seen = new Map<string, string>();
    for (const p of run.pairs) {
      seen.set(p.source_uuid, p.source_name);
      seen.set(p.target_uuid, p.target_name);
    }
    return [...seen.entries()].map(([uuid, name]) => ({ uuid, name })).sort((a, b) => a.name.localeCompare(b.name));
  }, [run]);
  const byKey = new Map(run.pairs.map((p) => [`${p.source_uuid}>${p.target_uuid}`, p]));
  const columns: ColumnsType<{ uuid: string; name: string }> = [
    { title: t("test.matrix"), dataIndex: "name", fixed: "left" },
    ...nodes.map((target) => ({
      title: target.name,
      key: target.uuid,
      align: "center" as const,
      render: (_: unknown, source: { uuid: string }) => {
        if (source.uuid === target.uuid) return <Typography.Text type="secondary">—</Typography.Text>;
        const pair = byKey.get(`${source.uuid}>${target.uuid}`);
        return pair ? <PairTag pair={pair} bandwidth={run.bandwidth} slow={slow(pair)} onOpen={() => onOpen(pair)} /> : null;
      },
    })),
  ];
  return <Table rowKey="uuid" size="small" pagination={false} columns={columns} dataSource={nodes} scroll={{ x: true }} />;
}

function PairList({ run, onOpen, slow }: { run: NetworkTestRun; onOpen: (p: NetworkTestPair) => void; slow: (p: NetworkTestPair) => boolean }) {
  const { t } = useTranslation("networks");
  return (
    <Table<NetworkTestPair>
      rowKey={(p) => `${p.source_uuid}>${p.target_uuid}`}
      size="small"
      pagination={run.pairs.length > 50 ? { defaultPageSize: 50 } : false}
      dataSource={run.pairs}
      columns={[
        { title: t("test.source"), dataIndex: "source_name" },
        { title: t("test.target"), dataIndex: "target_name" },
        { title: t("test.result"), key: "result", render: (_, p) => <PairTag pair={p} bandwidth={run.bandwidth} slow={slow(p)} onOpen={() => onOpen(p)} /> },
      ]}
    />
  );
}

/** One pair: its endpoints, and rail by rail for RDMA, with a way to drop a failing rail. */
function PairDetail({ network, run, pair, open, onClose, afterClose, onChanged }: {
  network: NetworkDetail;
  run: NetworkTestRun;
  pair: NetworkTestPair | null;
  open: boolean;
  onClose: () => void;
  afterClose: () => void;
  onChanged: (detail: NetworkDetail) => void;
}) {
  const { t } = useTranslation("networks");
  if (!pair) return null;
  const rails = pair.rails ?? [];
  const pairFailure = failureText(t, pair);
  return (
    <Modal open={open} title={`${pair.source_name} → ${pair.target_name}`} onCancel={onClose}
      afterClose={afterClose} footer={null} width={860} destroyOnHidden>
      <Descriptions column={1} size="small" style={{ marginBottom: 12 }}>
        <Descriptions.Item label={t("test.result")}><Tag color={statusColor[pair.status]}>{t(`test.status.${pair.status}`)}</Tag></Descriptions.Item>
        {pair.target && <Descriptions.Item label={t("test.targetAddress")}><Typography.Text code>{pair.target}</Typography.Text></Descriptions.Item>}
        {pair.reachable && pair.rtt_ms_p50 > 0 && <Descriptions.Item label="RTT">{t("test.rtt", { p50: pair.rtt_ms_p50.toFixed(2), p99: pair.rtt_ms_p99.toFixed(2) })}</Descriptions.Item>}
        {run.bandwidth && pair.gbps > 0 && <Descriptions.Item label={t("test.bandwidth")}>{t("test.gbps", { value: pair.gbps.toFixed(1) })}</Descriptions.Item>}
        {pairFailure && <Descriptions.Item label={t("test.problemWhy")}>{pairFailure}</Descriptions.Item>}
      </Descriptions>
      {rails.length > 0 && (
        <Table<NetworkTestRail>
          rowKey={(r) => `${r.rail}>${r.target_rail ?? ""}`}
          size="small"
          pagination={false}
          dataSource={rails}
          scroll={{ x: true }}
          columns={[
            { title: t("test.sourceHCA"), key: "source", render: (_, r) => (r.rail ? <Space size={6}><Typography.Text code>{r.rail}</Typography.Text><Typography.Text type="secondary">{r.source_ip || "—"}</Typography.Text></Space> : "—") },
            { title: t("test.targetHCA"), key: "target", render: (_, r) => (r.target_rail ? <Space size={6}><Typography.Text code>{r.target_rail}</Typography.Text><Typography.Text type="secondary">{r.target_ip || "—"}</Typography.Text></Space> : "—") },
            {
              title: t("test.result"),
              key: "result",
              render: (_, r) => (railOK(r)
                ? <Tag color="green">{t("test.gbps", { value: r.gbps.toFixed(1) })}</Tag>
                : <Space size={4} wrap><Tag color="red">{t("test.failed")}</Tag><Typography.Text>{failureText(t, r, railAtFault(pair, r).name)}</Typography.Text></Space>),
            },
            {
              title: t("table.actions"),
              key: "actions",
              render: (_, r) => {
                if (railOK(r) || !usesRails(network.type)) return null;
                const at = railAtFault(pair, r);
                return at.hca ? <RemoveHCAButton network={network} hca={at.hca} nodeUUID={at.uuid} nodeName={at.name} onChanged={onChanged} label={t("rail.removeShort")} /> : null;
              },
            },
          ]}
        />
      )}
      {rails.length > 0 && <Typography.Paragraph type="secondary" style={{ marginTop: 12, marginBottom: 0 }}>{t("test.retestHint")}</Typography.Paragraph>}
      <Space style={{ marginTop: 12, width: "100%", justifyContent: "flex-end" }}><Button onClick={onClose}>{t("actions.close")}</Button></Space>
    </Modal>
  );
}

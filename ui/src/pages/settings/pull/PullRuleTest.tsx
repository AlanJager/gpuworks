import { useRef, useState } from "react";
import { Alert, Button, Form, Input, Space, Table, Tag, Tooltip, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { api, type Node, type SourceProbeOutcome } from "../../../api";
import { APIError } from "../../../apiError";
import { nodeDisplayName } from "../../../nodeDisplay";
import NodePicker from "../../../nodes/NodePicker";
import { pullErrorText } from "./pullFormat";

/** The repository asked about when the operator does not name one (the server's own default). */
export const DEFAULT_TEST_REPO = "Qwen/Qwen3-0.6B";
/** How many nodes one test may ask. */
export const MAX_TEST_NODES = 5;

interface Row {
  key: string;
  node: string;
  pending?: boolean;
  nodeError?: string;
  outcome?: SourceProbeOutcome;
}

/** One node's answer: still running, the addresses it tried, or why it could not be asked. */
interface NodeAnswer {
  uuid: string;
  name: string;
  pending: boolean;
  outcomes: SourceProbeOutcome[];
  error?: string;
}

function rowsOf(answers: NodeAnswer[]): Row[] {
  return answers.flatMap((answer): Row[] => {
    if (answer.pending || answer.error !== undefined || answer.outcomes.length === 0) {
      return [{ key: answer.uuid, node: answer.name, pending: answer.pending, nodeError: answer.error ?? "" }];
    }
    return answer.outcomes.map((outcome, index) => ({ key: `${answer.uuid}:${index}`, node: answer.name, outcome }));
  });
}

function StatusCell({ outcome }: { outcome: SourceProbeOutcome }) {
  const { t } = useTranslation("settings");
  switch (outcome.status) {
    case "ok":
      return <Tag color="green">{t("pull.rules.test.ok")}</Tag>;
    case "auth_required":
      return (
        <Tooltip title={t("pull.rules.test.authHelp", { code: outcome.code ?? "" })}>
          <Tag color="orange">{t("pull.rules.test.authRequired")}</Tag>
        </Tooltip>
      );
    case "http_error":
      return <Tag color="red">{t("pull.rules.test.httpError", { code: outcome.code ?? "" })}</Tag>;
    default:
      return (
        <Tooltip title={outcome.error || undefined}>
          <Tag color="red">{t("pull.rules.test.unreachable")}</Tag>
        </Tooltip>
      );
  }
}

/**
 * PullRuleTest asks 1 to 5 online nodes whether they reach the addresses of
 * the site rule being edited (the unsaved draft, POST /pull/source-rules/test,
 * one request per node, all at once) and lists, per node and address, what
 * each answered as it arrives: usable, wanting a login (401/403, a credential
 * problem and not a wrong address), another HTTP status, or unreachable.
 * Nothing is saved.
 */
export default function PullRuleTest({
  match,
  endpoints,
  allowUpstream,
}: {
  match: string;
  endpoints: string[];
  allowUpstream: boolean;
}) {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const [nodes, setNodes] = useState<Node[]>([]);
  const [repo, setRepo] = useState(DEFAULT_TEST_REPO);
  const [answers, setAnswers] = useState<NodeAnswer[] | null>(null);
  const [error, setError] = useState("");
  // A newer run replaces the answers of one still in flight.
  const runID = useRef(0);

  const run = async () => {
    const id = ++runID.current;
    const asked = nodes.map((node): NodeAnswer => ({ uuid: node.uuid, name: nodeDisplayName(node) || node.uuid, pending: true, outcomes: [] }));
    setError("");
    setAnswers(asked);
    const settle = (uuid: string, update: Partial<NodeAnswer>) => {
      if (runID.current !== id) return;
      setAnswers((current) => current?.map((answer) => (answer.uuid === uuid ? { ...answer, ...update, pending: false } : answer)) ?? null);
    };
    const body = {
      match: match.trim(),
      endpoints: endpoints.map((item) => item.trim()).filter(Boolean),
      allow_upstream: allowUpstream,
      repo: repo.trim() || undefined,
    };
    await Promise.all(
      asked.map(async (answer) => {
        try {
          const result = await api.testPullSourceRule({ ...body, node_uuid: answer.uuid });
          settle(answer.uuid, { name: result.node_name || answer.name, outcomes: result.outcomes });
        } catch (err) {
          // A rejected draft is the rule's problem, not the node's: say it once.
          if (err instanceof APIError && err.status === 400) {
            if (runID.current === id) {
              setAnswers(null);
              setError(pullErrorText(t, tCommon, err));
            }
            return;
          }
          settle(answer.uuid, { error: pullErrorText(t, tCommon, err) });
        }
      }),
    );
  };

  const running = answers?.some((answer) => answer.pending) ?? false;
  const rows = answers ? rowsOf(answers) : [];
  return (
    <section aria-label={t("pull.rules.test.title")} style={{ marginBottom: 16 }}>
      <Typography.Title level={5}>{t("pull.rules.test.title")}</Typography.Title>
      <Typography.Paragraph type="secondary">{t("pull.rules.test.help")}</Typography.Paragraph>
      <Form layout="vertical" component={false}>
        <Form.Item label={t("pull.rules.test.nodes")} extra={t("pull.rules.test.nodesHelp", { max: MAX_TEST_NODES })}>
          <NodePicker
            mode="multiple"
            value={nodes.map((node) => node.uuid)}
            onChange={(_, picked) => setNodes(picked)}
            filter={{ state: ["online"] }}
            maxCount={MAX_TEST_NODES}
            placeholder={t("pull.rules.test.nodesPlaceholder")}
            notFoundContent={t("pull.rules.test.noOnlineNodes")}
            aria-label={t("pull.rules.test.nodes")}
            style={{ width: "100%" }}
          />
        </Form.Item>
        <Form.Item label={t("pull.rules.test.repo")} htmlFor="pull-rule-test-repo" extra={t("pull.rules.test.repoHelp")}>
          <Input id="pull-rule-test-repo" autoComplete="off" value={repo} onChange={(event) => setRepo(event.target.value)} />
        </Form.Item>
        <Space>
          <Button loading={running} disabled={nodes.length === 0 || !match.trim()} onClick={() => void run()}>
            {t("pull.rules.test.run")}
          </Button>
        </Space>
      </Form>
      {error && <Alert style={{ marginTop: 8 }} role="alert" type="error" showIcon message={error} />}
      {answers && (
        <Table<Row>
          style={{ marginTop: 12 }}
          size="small"
          rowKey="key"
          pagination={false}
          dataSource={rows}
          locale={{ emptyText: t("pull.rules.test.empty") }}
          aria-label={t("pull.rules.test.resultLabel")}
          columns={[
            { title: t("pull.rules.test.node"), dataIndex: "node" },
            {
              title: t("pull.rules.test.address"),
              key: "address",
              render: (_, row) => {
                if (row.outcome) return row.outcome.base;
                if (row.pending) return <Typography.Text type="secondary">{t("pull.rules.test.pending")}</Typography.Text>;
                return <Typography.Text type="danger">{row.nodeError || t("pull.rules.test.noAnswer")}</Typography.Text>;
              },
            },
            { title: t("pull.rules.test.result"), key: "result", render: (_, row) => (row.outcome ? <StatusCell outcome={row.outcome} /> : null) },
            {
              title: t("pull.rules.test.latency"),
              key: "latency",
              render: (_, row) => (row.outcome && row.outcome.latency_ms > 0 ? t("pull.rules.test.ms", { ms: row.outcome.latency_ms }) : ""),
            },
          ]}
        />
      )}
    </section>
  );
}

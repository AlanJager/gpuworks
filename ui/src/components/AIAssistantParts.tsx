import { Badge } from "./ui/badge";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "./ui/table";
import { Tooltip } from "./ui/tooltip";
import { Notice } from "../ui-kit/Notice";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import type { AIFinding, AIModelFit, AIStreamEvent } from "../api";
import { formatBytes } from "../format";

// The deterministic half of the assistant: what the server knows without an
// LLM, shown before the answer and on its own when no AI service is set up.

/** FindingCards renders the known failures an instance's logs match. */
export function FindingCards({ findings }: { findings: AIFinding[] }) {
  const { t } = useTranslation("ai");
  if (findings.length === 0) {
    return <span className="text-fg-muted text-label">{t("finding.none")}</span>;
  }
  return (
    <div className="flex w-full flex-col gap-2">
      {findings.map((f) => (
        <Notice
          key={f.code}
          tone="warning"
          role="alert"
          title={
            <span className="inline-flex flex-wrap items-center gap-1.5">
              <span>{t(`finding.${f.code}.title`, { defaultValue: f.code })}</span>
              <Badge size="sm" color="amber">{t("finding.ruleTag")}</Badge>
            </span>
          }
        >
          <div className="flex w-full flex-col gap-1.5">
            <span>{t(`finding.${f.code}.fix`, { defaultValue: "" })}</span>
            <pre aria-label={t("finding.evidence")} className="m-0 whitespace-pre-wrap break-all rounded-sm bg-emphasis px-2 py-1.5 text-body">
              {f.evidence.map((e) => e.line).join("\n")}
            </pre>
          </div>
        </Notice>
      ))}
      <span className="text-fg-muted text-label">{t("finding.verifyNote")}</span>
    </div>
  );
}

/** FitTable renders the estimator's fit of a model on each card type. */
export function FitTable({ fit }: { fit: AIModelFit }) {
  const { t } = useTranslation("ai");
  if (fit.rows.length === 0) {
    return <span className="text-fg-muted text-label">{t("fit.noNodes")}</span>;
  }
  const gib = (v: number) => formatBytes(v, { minUnit: "GiB", maxUnit: "GiB" });
  const columns: { key: string; title: string; render: (r: AIModelFit["rows"][number]) => ReactNode }[] = [
    {
      key: "card", title: t("fit.card"), render: (r) => (
        <div className="flex flex-col">
          <span>{r.name} <span className="text-fg-muted">×{r.cards} · {t("fit.nodes", { count: r.nodes })}</span></span>
          {r.unusable > 0 && <span className="text-fg-danger text-label">{t("fit.unusable", { count: r.unusable })}</span>}
        </div>
      ),
    },
    {
      key: "fits", title: t("fit.runs"), render: (r) => (
        <div className="flex flex-col">
          {r.fits ? <span className="text-fg-success">{t("fit.yes", { tp: r.tp })}</span> : <span className="text-fg-danger">{t("fit.no")}</span>}
          {!r.dtype_supported && <Tooltip content={r.dtype_note}><span className="w-fit text-fg-warning text-label">{t("fit.dtypeUnsupported", { dtype: fit.dtype || "" })}</span></Tooltip>}
        </div>
      ),
    },
    { key: "link", title: t("fit.link"), render: (r) => (r.fast_link_cards > 1 ? t("fit.fastLink", { count: r.fast_link_cards }) : r.fast_link_cards === 1 ? t("fit.pcieOnly") : "—") },
    { key: "need", title: t("fit.perGPU"), render: (r) => `${gib(r.per_gpu_bytes)} / ${gib(r.usable_bytes)}` },
    { key: "headroom", title: t("fit.headroom"), render: (r) => (r.fits ? gib(r.headroom_bytes) : "—") },
  ];
  return (
    <div className="flex w-full flex-col gap-1.5">
      <span className="inline-flex items-center gap-1.5">
        <span className="font-semibold">{t("fit.title")}</span>
        <Badge size="sm" color="blue">{t("fit.estimatorTag")}</Badge>
      </span>
      <Table size="sm">
        <TableHeader>
          <TableRow>{columns.map((c) => <TableHead key={c.key}>{c.title}</TableHead>)}</TableRow>
        </TableHeader>
        <TableBody>
          {fit.rows.map((r) => (
            <TableRow key={`${r.vendor}/${r.name}`}>{columns.map((c) => <TableCell key={c.key}>{c.render(r)}</TableCell>)}</TableRow>
          ))}
        </TableBody>
      </Table>
      <span className="text-fg-muted text-label">
        {t("fit.note", { util: Math.round(fit.gpu_memory_utilization * 100) })}
      </span>
    </div>
  );
}

/** One tool call of a streamed answer, as its step events last said. */
export interface LiveStep {
  id: number;
  tool: string;
  args: string;
  status: "running" | "done" | "error";
  ms: number;
  error?: string;
}

/** StepList shows the tool calls the AI made, live: running, done or failed. */
/**
 * stepDetail is what one tool call read, for a person: the endpoint and jq
 * filter of an API read, the words of a search, else the raw arguments.
 */
export function stepDetail(tool: string, args: string): string {
  let parsed: Record<string, unknown>;
  try {
    parsed = JSON.parse(args || "{}");
  } catch {
    return args;
  }
  if (tool === "call_api" && typeof parsed.path === "string") {
    const query = parsed.query && typeof parsed.query === "object" ? new URLSearchParams(Object.entries(parsed.query as Record<string, unknown>).map(([k, v]) => [k, String(v)])).toString() : "";
    return `GET ${parsed.path}${query ? `?${query}` : ""}${typeof parsed.jq === "string" && parsed.jq ? ` · jq: ${parsed.jq}` : ""}`;
  }
  if ((tool === "search_api" || tool === "search_metrics") && typeof parsed.query === "string") return parsed.query;
  if ((tool === "get_gpu_topology" || tool === "get_node_connection_diagnostics") && typeof parsed.node_uuid === "string") return parsed.node_uuid;
  if (tool === "compare_with_healthy_replica" && typeof parsed.instance_uuid === "string") return parsed.instance_uuid;
  if (tool === "cluster_overview") return "";
  return args;
}

/**
 * selfCorrectable reports a tool error the model is handed the means to fix
 * on its own next step - a jq filter or arguments it wrote wrong, an endpoint
 * it guessed - rather than a failure of the platform.
 */
export function selfCorrectable(error: string): boolean {
  return /^(jq: |arguments are not valid JSON|no GET endpoint |path must start with |no node is named )/.test(error);
}

export function StepList({ steps }: { steps: LiveStep[] }) {
  const { t } = useTranslation("ai");
  if (steps.length === 0) return null;
  return (
    <ul aria-label={t("trace.steps")} style={{ listStyle: "none", margin: 0, padding: 0, fontSize: 12 }}>
      {steps.map((s) => (
        <li key={s.id} style={{ padding: "2px 0" }}>
          {/* One line per step: the tool tag and the time never wrap or
              shrink; only what it read is cut. A note or error goes on its
              own line below, so it cannot squeeze the row. */}
          <div style={{ display: "flex", gap: 6, alignItems: "center", minWidth: 0 }}>
            <span aria-hidden style={{ width: 14, flex: "none" }}>{s.status === "running" ? "…" : s.status === "done" ? "✓" : s.error && selfCorrectable(s.error) ? "↻" : "✗"}</span>
            <code className="flex-none whitespace-nowrap rounded-sm bg-emphasis px-1 font-mono text-label">{t(`tool.${s.tool}`, { defaultValue: s.tool })}</code>
            <Tooltip content={s.args}><span className="text-fg-muted flex-1 min-w-0 truncate">{stepDetail(s.tool, s.args)}</span></Tooltip>
            <span className="text-fg-muted flex-none whitespace-nowrap">{s.status === "running" ? t("trace.running") : `${s.ms} ms`}</span>
          </div>
          {s.error && (
            <div style={{ paddingLeft: 20 }}>
              {selfCorrectable(s.error)
                ? <span className="text-fg-muted text-label" title={s.error}>{t("trace.selfCorrect")}</span>
                : <span className="text-fg-danger text-label break-words">{s.error}</span>}
            </div>
          )}
        </li>
      ))}
    </ul>
  );
}

/** AnswerSummary says what a finished answer cost. */
export function AnswerSummary({ done, tools }: { done: Pick<AIStreamEvent & { type: "done" }, "calls" | "usage" | "first_text_ms" | "total_ms">; tools: number }) {
  const { t } = useTranslation("ai");
  const seconds = (ms: number) => (ms / 1000).toFixed(1);
  return (
    <span className="text-fg-muted text-label">
      {t("trace.summary", {
        first: seconds(done.first_text_ms), total: seconds(done.total_ms), tools, calls: done.calls,
        tokens: done.usage.prompt_tokens + done.usage.completion_tokens,
      })}
      {tools === 0 && ` · ${t("trace.noTools")}`}
    </span>
  );
}

// tailChars bounds what ReasoningTail renders: a long chain of thought grows
// to thousands of tokens, and only its last lines are worth a glance.
const tailChars = 600;

/**
 * ReasoningTail shows the newest lines of the model's reasoning while it
 * thinks and no answer text has come yet: a reasoning model can think for
 * twenty seconds, and a spinner alone reads as a hang.
 */
export function ReasoningTail({ text, label }: { text: string; label: string }) {
  const tail = text.length > tailChars ? "…" + text.slice(-tailChars) : text;
  return (
    <div
      role="log"
      aria-label={label}
      style={{
        maxHeight: "4.8em", overflow: "hidden", display: "flex", flexDirection: "column", justifyContent: "flex-end",
        fontSize: 12, lineHeight: "1.6em", color: "rgba(0,0,0,0.45)", whiteSpace: "pre-wrap", wordBreak: "break-word",
        borderLeft: "2px solid rgba(0,0,0,0.12)", paddingLeft: 8,
        maskImage: "linear-gradient(to bottom, transparent, black 1.6em)", WebkitMaskImage: "linear-gradient(to bottom, transparent, black 1.6em)",
      }}
    >
      <div>{tail}</div>
    </div>
  );
}

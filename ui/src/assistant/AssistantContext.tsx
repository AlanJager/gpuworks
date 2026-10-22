import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { api, type AIAskRequest, type AIFinding, type AIModelFit, type AIStreamEvent } from "../api";
import { APIError } from "../apiError";
import type { LiveStep } from "../components/AIAssistantParts";

/**
 * What the assistant is asked to do: a kind of subject and an intent. Each
 * task has its own seed and prompt on the server, so a user starts one with
 * a click; "ask" is a free question, about a subject when one is chosen.
 */
export type AssistantTask = "instance.diagnose" | "model.fit" | "deployment.unscheduled" | "node.health" | "ask";

/** The resource a task is about, by reference, with the name to show. */
export type AssistantSubject =
  | { kind: "instance"; uuid: string; label: string }
  | { kind: "model"; id: number; label: string }
  | { kind: "deployment"; id: number; label: string }
  | { kind: "node"; uuid: string; label: string };

/** A resource the current page shows, the tasks it offers, and what about it needs attention. */
export interface PageSubject {
  subject: AssistantSubject;
  tasks: AssistantTask[];
  attention?: string;
}

export type Facts = { findings: AIFinding[] } | { fit: AIModelFit };

export type FactsState =
  | { status: "none" | "loading" }
  | { status: "done"; value: Facts }
  | { status: "error"; message: string };

export type DoneInfo = Extract<AIStreamEvent, { type: "done" }>;

/** What has streamed in so far. */
export type Live = { text: string; reasoning: string; steps: LiveStep[] };

export type AnswerState =
  | { status: "unconfigured" }
  | { status: "streaming" | "stopped"; live: Live }
  | { status: "done"; live: Live; done: DoneInfo }
  | { status: "error"; live: Live; message: string };

/** One question and its answer in the thread. */
export interface Turn {
  id: number;
  task: AssistantTask;
  subject?: AssistantSubject;
  question?: string;
  facts: FactsState;
  answer: AnswerState;
}

const emptyLive: Live = { text: "", reasoning: "", steps: [] };

/** apply folds one stream event into what is shown. */
export function applyEvent(live: Live, ev: AIStreamEvent): Live {
  switch (ev.type) {
    case "text":
      return { ...live, text: live.text + ev.delta };
    case "reasoning":
      return { ...live, reasoning: live.reasoning + ev.delta };
    case "step": {
      // Text the model wrote before calling tools ("let me check the logs")
      // ends a paragraph; the answer that follows starts a new one.
      const text = live.text && !live.text.endsWith("\n\n") ? live.text + "\n\n" : live.text;
      const step: LiveStep = { id: ev.id, tool: ev.tool, args: ev.args, status: ev.status, ms: ev.ms, error: ev.error };
      const at = live.steps.findIndex((s) => s.id === ev.id);
      const steps = at < 0 ? [...live.steps, step] : live.steps.map((s, i) => (i === at ? step : s));
      return { ...live, text, steps };
    }
    default:
      return live;
  }
}

function subjectRefs(subject?: AssistantSubject): Partial<AIAskRequest> {
  switch (subject?.kind) {
    case "instance":
      return { instance_uuid: subject.uuid };
    case "model":
      return { model_id: subject.id };
    case "deployment":
      return { deployment_id: subject.id };
    case "node":
      return { node_uuid: subject.uuid };
    default:
      return {};
  }
}

export function sameSubject(a?: AssistantSubject, b?: AssistantSubject): boolean {
  return !!a && !!b && a.kind === b.kind && JSON.stringify(subjectRefs(a)) === JSON.stringify(subjectRefs(b));
}

function factsFor(task: AssistantTask, subject: AssistantSubject | undefined, signal: AbortSignal): Promise<Facts> | null {
  if (task === "instance.diagnose" && subject?.kind === "instance") return api.aiInstanceFindings(subject.uuid, signal).then((list) => ({ findings: list.items }));
  if (task === "model.fit" && subject?.kind === "model") return api.aiModelFit(subject.id, signal).then((fit) => ({ fit }));
  return null;
}

interface AssistantValue {
  open: boolean;
  setOpen: (open: boolean) => void;
  turns: Turn[];
  pageSubjects: PageSubject[];
  /** start opens the panel on a new thread running task about subject. */
  start: (task: AssistantTask, subject?: AssistantSubject) => void;
  /** ask continues the thread with a question, or starts a free one about subject. */
  ask: (question: string, subject?: AssistantSubject) => void;
  stop: () => void;
  retry: (turnId: number) => void;
  reset: () => void;
  register: (key: string, subjects: PageSubject[]) => void;
  unregister: (key: string) => void;
}

const AssistantContext = createContext<AssistantValue | null>(null);

// AssistantProvider holds the assistant across pages: the thread, the panel's
// visibility and what the current page offers. It lives in the app shell, so
// navigating keeps the conversation.
export function AssistantProvider({ children }: { children: ReactNode }) {
  const { t, i18n } = useTranslation("ai");
  const [open, setOpen] = useState(false);
  const [turns, setTurns] = useState<Turn[]>([]);
  const [registry, setRegistry] = useState<Record<string, PageSubject[]>>({});
  const nextId = useRef(1);
  const active = useRef<{ id: number; ac: AbortController } | null>(null);
  const turnsRef = useRef<Turn[]>([]);
  turnsRef.current = turns;

  useEffect(() => () => active.current?.ac.abort(), []);
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "i") {
        event.preventDefault();
        setOpen((v) => !v);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const update = useCallback((id: number, change: (turn: Turn) => Turn) => {
    setTurns((all) => all.map((turn) => (turn.id === id ? change(turn) : turn)));
  }, []);

  // run streams one turn's answer, and its facts when the task has any.
  const run = useCallback((turn: Turn, history: { role: "user" | "assistant"; content: string }[]) => {
    active.current?.ac.abort();
    const ac = new AbortController();
    active.current = { id: turn.id, ac };
    const locale = i18n.resolvedLanguage === "zh" ? "zh" : "en";
    const facts = history.length === 0 ? factsFor(turn.task, turn.subject, ac.signal) : null;
    update(turn.id, (x) => ({ ...x, facts: { status: facts ? "loading" : "none" }, answer: { status: "streaming", live: emptyLive } }));
    facts?.then(
      (value) => !ac.signal.aborted && update(turn.id, (x) => ({ ...x, facts: { status: "done", value } })),
      (e) => !ac.signal.aborted && update(turn.id, (x) => ({ ...x, facts: { status: "error", message: (e as Error).message } })),
    );
    const body: AIAskRequest = { task: turn.task, ...subjectRefs(turn.subject), locale };
    if (turn.question) body.question = turn.question;
    if (history.length) body.history = history;
    const liveOf = (x: Turn) => ("live" in x.answer ? x.answer.live : emptyLive);
    let ended = false;
    api.aiAskStream(body, (ev) => {
      if (ac.signal.aborted) return;
      if (ev.type === "done" || ev.type === "error") ended = true;
      update(turn.id, (x) => {
        const live = liveOf(x);
        if (ev.type === "done") return { ...x, answer: { status: "done", live, done: ev } };
        if (ev.type === "error") return { ...x, answer: { status: "error", live, message: ev.error } };
        return { ...x, answer: { status: "streaming", live: applyEvent(live, ev) } };
      });
    }, ac.signal).then(
      () => {
        // A stream that closed without done or error is not an answer.
        if (!ac.signal.aborted && !ended) update(turn.id, (x) => ({ ...x, answer: { status: "error", live: liveOf(x), message: t("streamCut") } }));
      },
      (e) => {
        if (ac.signal.aborted) return;
        if (e instanceof APIError && e.code === "AI_ENDPOINT_NOT_CONFIGURED") update(turn.id, (x) => ({ ...x, answer: { status: "unconfigured" } }));
        else update(turn.id, (x) => ({ ...x, answer: { status: "error", live: liveOf(x), message: (e as Error).message || t("requestFailed") } }));
      },
    );
  }, [i18n, t, update]);

  // historyBefore is the thread up to a turn: what each earlier turn asked
  // (the task's own wording when it was a click) and what came back.
  const historyBefore = useCallback((all: Turn[], id: number) => {
    const out: { role: "user" | "assistant"; content: string }[] = [];
    for (const turn of all) {
      if (turn.id === id) break;
      out.push({ role: "user", content: turn.question || t(`task.${turn.task}.title`) });
      if ("live" in turn.answer && turn.answer.live.text) out.push({ role: "assistant", content: turn.answer.live.text });
    }
    return out;
  }, [t]);

  const start = useCallback((task: AssistantTask, subject?: AssistantSubject) => {
    const turn: Turn = { id: nextId.current++, task, subject, facts: { status: "none" }, answer: { status: "streaming", live: emptyLive } };
    setTurns([turn]);
    setOpen(true);
    run(turn, []);
  }, [run]);

  const ask = useCallback((question: string, subject?: AssistantSubject) => {
    const q = question.trim();
    if (!q) return;
    const thread = turnsRef.current;
    const first = thread[0];
    // A follow-up keeps the thread's task and subject; a first question is
    // free, about the subject the user picked.
    const turn: Turn = first
      ? { id: nextId.current++, task: first.task, subject: first.subject, question: q, facts: { status: "none" }, answer: { status: "streaming", live: emptyLive } }
      : { id: nextId.current++, task: "ask", subject, question: q, facts: { status: "none" }, answer: { status: "streaming", live: emptyLive } };
    const all = [...thread, turn];
    setTurns(all);
    setOpen(true);
    run(turn, historyBefore(all, turn.id));
  }, [historyBefore, run]);

  const stop = useCallback(() => {
    const current = active.current;
    if (!current) return;
    current.ac.abort();
    update(current.id, (x) => ({
      ...x,
      facts: x.facts.status === "loading" ? { status: "none" } : x.facts,
      answer: { status: "stopped", live: "live" in x.answer ? x.answer.live : emptyLive },
    }));
  }, [update]);

  const retry = useCallback((id: number) => {
    const all = turnsRef.current;
    const turn = all.find((x) => x.id === id);
    if (turn) run(turn, historyBefore(all, id));
  }, [historyBefore, run]);

  const reset = useCallback(() => {
    active.current?.ac.abort();
    active.current = null;
    setTurns([]);
  }, []);

  const register = useCallback((key: string, subjects: PageSubject[]) => setRegistry((r) => ({ ...r, [key]: subjects })), []);
  const unregister = useCallback((key: string) => setRegistry((r) => {
    const next = { ...r };
    delete next[key];
    return next;
  }), []);

  const pageSubjects = useMemo(() => Object.values(registry).flat(), [registry]);
  const value = useMemo<AssistantValue>(
    () => ({ open, setOpen, turns, pageSubjects, start, ask, stop, retry, reset, register, unregister }),
    [open, turns, pageSubjects, start, ask, stop, retry, reset, register, unregister],
  );
  return <AssistantContext.Provider value={value}>{children}</AssistantContext.Provider>;
}

/** useOptionalAssistant is the assistant when the app shell provides one, else null. */
export function useOptionalAssistant(): AssistantValue | null {
  return useContext(AssistantContext);
}

export function useAssistant(): AssistantValue {
  const value = useContext(AssistantContext);
  if (!value) throw new Error("useAssistant needs an AssistantProvider");
  return value;
}

let registrations = 0;

/**
 * useAssistantSubjects tells the assistant what the calling page shows, so
 * its panel offers those tasks and points at what needs attention. Pass
 * null while nothing is shown.
 */
export function useAssistantSubjects(subjects: PageSubject[] | null) {
  const value = useContext(AssistantContext);
  const key = useRef("");
  if (!key.current) key.current = `page-${++registrations}`;
  const signature = JSON.stringify(subjects);
  useEffect(() => {
    if (!value) return;
    const id = key.current;
    if (subjects && subjects.length) value.register(id, subjects);
    else value.unregister(id);
    return () => value.unregister(id);
    // The signature carries the subjects' content; the register functions are stable.
  }, [signature, value?.register, value?.unregister]);
}

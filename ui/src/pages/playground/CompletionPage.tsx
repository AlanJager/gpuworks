import UnsupportedModel from "./UnsupportedModel";
import RequestDrawer from "./RequestDrawer";
import { composeRequest } from "./compose";
import { Alert, Button, Empty, Input, InputNumber, Select, Space, Typography } from "antd";
import { useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { invokeJSON, streamCompletion } from "./api";
import { modelsForOperation, operationOf } from "./operations";
import type { PlaygroundCatalog, PlaygroundOperation, Usage } from "./types";

const OPERATION = "openai.completions";
const DEFAULT_PROMPT = "The capital of France is";

// The text of a non-streamed completion reply (choices[0].text).
function replyText(reply: unknown): string {
  const choices = (reply as { choices?: Array<{ text?: unknown }> } | undefined)?.choices;
  return typeof choices?.[0]?.text === "string" ? choices[0].text : "";
}

// CompletionPage sends a raw prompt to a model that declares text completion
// (/v1/completions) and shows its continuation: no chat template is applied,
// so the prompt is whatever format the model expects.
export default function CompletionPage({ catalog }: { catalog: PlaygroundCatalog }) {
  const { t } = useTranslation("playground");
  const [search, setSearch] = useSearchParams();
  const models = useMemo(() => modelsForOperation(catalog.models, OPERATION), [catalog.models]);
  const requested = search.get("model") || "";
  const modelID = requested || models[0]?.id || "";
  const operation = operationOf(models.find((item) => item.id === modelID), OPERATION);
  const [prompt, setPrompt] = useState(DEFAULT_PROMPT);
  const [maxTokens, setMaxTokens] = useState<number>(256);
  const [text, setText] = useState("");
  const [usage, setUsage] = useState<Usage>();
  const [error, setError] = useState<string>();
  const [running, setRunning] = useState(false);
  const abort = useRef<AbortController>(undefined);

  useEffect(() => {
    const next = new URLSearchParams(search);
    next.set("operation", OPERATION);
    if (modelID) next.set("model", modelID); else next.delete("model");
    if (next.toString() !== search.toString()) setSearch(next, { replace: true });
  }, [modelID, search, setSearch]);
  useEffect(() => () => abort.current?.abort(), []);
  // A model that needs a particular format (PLaMo 2 Translate's tagged
  // prompt) declares a working one: start from it whenever that model is
  // picked, so a first Generate already shows what the model does.
  const declaredPrompt = operation?.example_input?.prompt;
  useEffect(() => {
    setPrompt(declaredPrompt ?? DEFAULT_PROMPT);
    setText(""); setUsage(undefined); setError(undefined);
  }, [modelID, declaredPrompt]);

  const body = (op: PlaygroundOperation) => composeRequest(op, modelID, { prompt, max_tokens: maxTokens }, {});

  const run = async (op: PlaygroundOperation, request: Record<string, unknown>) => {
    abort.current?.abort();
    const controller = new AbortController();
    abort.current = controller;
    setRunning(true); setText(""); setUsage(undefined); setError(undefined);
    try {
      if (op.mode === "sync") {
        const reply = await invokeJSON(op, modelID, { ...request, stream: false }, controller.signal);
        setText(replyText(reply));
        setUsage((reply as { usage?: Usage }).usage);
      } else {
        const result = await streamCompletion(op, request, (piece) => setText((current) => current + piece), controller.signal);
        setUsage(result.usage);
      }
    } catch (next) {
      if (!controller.signal.aborted) setError((next as Error).message);
    } finally {
      if (abort.current === controller) setRunning(false);
    }
  };

  return <div className="playground-operation-page">
    <Space wrap className="playground-operation-toolbar">
      <RequestDrawer operation={operation} model={modelID} body={operation ? body(operation) : {}} onSend={(raw) => { if (operation) void run(operation, JSON.parse(raw) as Record<string, unknown>); }} />
      <Select className="playground-model-select" value={modelID || undefined} placeholder={t("completion.modelPlaceholder")} options={models.map((item) => ({ value: item.id, label: item.label }))} onChange={(value) => { const next = new URLSearchParams(search); next.set("model", value); setSearch(next, { replace: true }); }} />
    </Space>
    {requested && !operation ? <UnsupportedModel modelID={requested} models={catalog.models} /> : !models.length ? <Empty description={t("completion.noModels")} /> : <div className="playground-operation-grid">
      <section>
        <Typography.Title level={5}><label htmlFor="playground-completion-prompt">{t("completion.prompt")}</label></Typography.Title>
        <Input.TextArea id="playground-completion-prompt" value={prompt} onChange={(event) => setPrompt(event.target.value)} autoSize={{ minRows: 6 }}
          onKeyDown={(event) => { if (event.key === "Enter" && (event.ctrlKey || event.metaKey) && operation && prompt.trim() && !running) { event.preventDefault(); void run(operation, body(operation)); } }} />
        <Typography.Text type="secondary">{t("completion.hint")}</Typography.Text>
        <Space>
          <label htmlFor="playground-completion-max-tokens">{t("completion.maxTokens")}</label>
          <InputNumber id="playground-completion-max-tokens" min={1} max={32768} value={maxTokens} onChange={(value) => setMaxTokens(Number(value) || 1)} />
        </Space>
        <Space>
          <Button type="primary" loading={running} disabled={!prompt.trim() || !operation} onClick={() => { if (operation) void run(operation, body(operation)); }}>{t("completion.run")}</Button>
          {running && <Button onClick={() => { abort.current?.abort(); setRunning(false); }}>{t("completion.stop")}</Button>}
        </Space>
      </section>
      <section>
        <Typography.Title level={5}>{t("completion.result")}</Typography.Title>
        {error && <Alert type="error" showIcon message={error} />}
        {text ? <pre>{text}</pre> : !error && <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t("completion.empty")} />}
        {usage && <Typography.Text type="secondary">{t("completion.usage", { prompt: usage.prompt_tokens ?? 0, completion: usage.completion_tokens ?? 0 })}</Typography.Text>}
      </section>
    </div>}
  </div>;
}

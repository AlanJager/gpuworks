import UnsupportedModel from "./UnsupportedModel";
import RequestDrawer from "./RequestDrawer";
import { composeRequest } from "./compose";
import { Button, Empty, Input, Segmented, Select, Space, Typography, message } from "antd";
import { useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { invokeJSON } from "./api";
import { modelsForOperation, operationOf } from "./operations";
import type { OperationType, PlaygroundCatalog } from "./types";

export default function VectorPage({ catalog }: { catalog: PlaygroundCatalog }) {
  const { t } = useTranslation("playground");
  const [search, setSearch] = useSearchParams();
  const kind = search.get("operation") === "openai.rerank" ? "rerank" : "embedding";
  const setKind = (value: "embedding" | "rerank") => {
    const next = new URLSearchParams(search);
    next.set("operation", value === "rerank" ? "openai.rerank" : "openai.embeddings");
    setSearch(next, { replace: true });
  };
  const operationType: OperationType = kind === "embedding" ? "openai.embeddings" : "openai.rerank";
  const models = useMemo(() => modelsForOperation(catalog.models, operationType), [catalog.models, operationType]);
  const requested = search.get("model") || "";
  const modelID = requested || models[0]?.id || "";
  const model = models.find((item) => item.id === modelID);
  const operation = operationOf(model, operationType);
  const [input, setInput] = useState("GPU inference");
  const [documents, setDocuments] = useState("GPU inference runs on accelerators.\nThis document is unrelated.");
  const [loading, setLoading] = useState(false);
  const [result, setResult] = useState<unknown>();

  useEffect(() => {
    const next = new URLSearchParams(search);
    next.set("operation", operationType);
    if (modelID) next.set("model", modelID); else next.delete("model");
    if (next.toString() !== search.toString()) setSearch(next, { replace: true });
  }, [modelID, operationType, search, setSearch]);

  const submit = async () => {
    if (!operation || !modelID) return;
    setLoading(true); setResult(undefined);
    try {
      setResult(await invokeJSON(operation, modelID, kind === "embedding"
        ? { input }
        : { query: input, documents: documents.split("\n").map((value) => value.trim()).filter(Boolean) }));
    } catch (error) { message.error((error as Error).message); }
    finally { setLoading(false); }
  };

  return <div className="playground-operation-page">
    <Space wrap className="playground-operation-toolbar">
      <RequestDrawer operation={operation} model={modelID} body={operation ? composeRequest(operation, modelID, kind === "embedding" ? { input } : { query: input, documents: documents.split("\n") }, {}) : {}} onSend={(raw) => { void invokeJSON(operation!, modelID, JSON.parse(raw)).then(setResult); }} /><Segmented value={kind} options={[{ value: "embedding", label: t("vector.embedding") }, { value: "rerank", label: t("vector.rerank") }]} onChange={(value) => setKind(value as "embedding" | "rerank")} />
      <Select className="playground-model-select" value={modelID || undefined} placeholder={t("vector.modelPlaceholder")} options={models.map((item) => ({ value: item.id, label: item.label }))} onChange={(value) => { const next = new URLSearchParams(search); next.set("model", value); setSearch(next, { replace: true }); }} />
    </Space>
    {requested && !operation ? <UnsupportedModel modelID={requested} models={catalog.models} /> : !models.length ? <Empty description={t("vector.noModels")} /> : <div className="playground-operation-grid">
      <section><Typography.Title level={5}>{kind === "embedding" ? t("vector.input") : t("vector.query")}</Typography.Title><Input.TextArea value={input} onChange={(event) => setInput(event.target.value)} autoSize={{ minRows: 4 }} />{kind === "rerank" && <><Typography.Title level={5}>{t("vector.documents")}</Typography.Title><Input.TextArea value={documents} onChange={(event) => setDocuments(event.target.value)} autoSize={{ minRows: 6 }} /></>}<Button type="primary" loading={loading} disabled={!input.trim()} onClick={() => void submit()}>{t("vector.run")}</Button></section>
      <section><Typography.Title level={5}>{t("vector.result")}</Typography.Title>{result ? <pre>{JSON.stringify(result, null, 2)}</pre> : <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t("vector.empty")} />}</section>
    </div>}
  </div>;
}

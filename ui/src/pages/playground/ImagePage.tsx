import UnsupportedModel from "./UnsupportedModel";
import RequestDrawer from "./RequestDrawer";
import { composeRequest, declaredValues } from "./compose";
import { Button, Empty, Input, Select, Space, Typography, message } from "antd";
import { useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { invokeJSON } from "./api";
import { modelsForOperation, operationOf } from "./operations";
import type { PlaygroundCatalog } from "./types";

export default function ImagePage({ catalog }: { catalog: PlaygroundCatalog }) {
  const { t } = useTranslation("playground");
  const [search, setSearch] = useSearchParams();
  const models = useMemo(() => modelsForOperation(catalog.models, "openai.image.generate"), [catalog.models]);
  const requested = search.get("model") || "";
  const modelID = requested || models[0]?.id || "";
  const operation = operationOf(models.find((model) => model.id === modelID), "openai.image.generate");
  const [prompt, setPrompt] = useState("A sunrise over mountains");
  const [size, setSize] = useState("1024x1024");
  const [loading, setLoading] = useState(false);
  const [result, setResult] = useState<unknown>();

  useEffect(() => {
    if (!modelID || requested === modelID) return;
    const next = new URLSearchParams(search); next.set("model", modelID); setSearch(next, { replace: true });
  }, [modelID, requested, search, setSearch]);
  const submit = async () => {
    if (!operation || !modelID) return;
    setLoading(true); setResult(undefined);
    try { setResult(await invokeJSON(operation, modelID, { prompt, size, response_format: "b64_json" })); }
    catch (error) { message.error((error as Error).message); }
    finally { setLoading(false); }
  };
  const imageURL = generatedImageURL(result);
  return <div className="playground-operation-page">
    <Space wrap className="playground-operation-toolbar"><Select className="playground-model-select" value={modelID || undefined} placeholder={t("image.modelPlaceholder")} options={models.map((model) => ({ value: model.id, label: model.label }))} onChange={(value) => { const next = new URLSearchParams(search); next.set("model", value); setSearch(next, { replace: true }); }} /><RequestDrawer operation={operation} model={modelID} body={operation ? composeRequest(operation, modelID, { prompt }, declaredValues(operation, { size })) : {}} onSend={(raw) => { void invokeJSON(operation!, modelID, JSON.parse(raw)).then(setResult); }} /><Select value={size} options={["512x512", "768x768", "1024x1024"].map((value) => ({ value }))} onChange={setSize} /></Space>
    {requested && !operation ? <UnsupportedModel modelID={requested} models={catalog.models} /> : !models.length ? <Empty description={t("image.noModels")} /> : <div className="playground-operation-grid"><section><Typography.Title level={5}>{t("image.prompt")}</Typography.Title><Input.TextArea value={prompt} onChange={(event) => setPrompt(event.target.value)} autoSize={{ minRows: 6 }} /><Button type="primary" loading={loading} disabled={!prompt.trim()} onClick={() => void submit()}>{t("image.generate")}</Button></section><section><Typography.Title level={5}>{t("image.result")}</Typography.Title>{imageURL ? <img className="playground-generated-image" src={imageURL} alt={prompt} /> : result ? <pre>{JSON.stringify(result, null, 2)}</pre> : <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t("image.empty")} />}</section></div>}
  </div>;
}

function generatedImageURL(result: unknown): string {
  if (!result || typeof result !== "object") return "";
  const data = (result as { data?: Array<{ b64_json?: string; url?: string }> }).data?.[0];
  if (data?.b64_json) return `data:image/png;base64,${data.b64_json}`;
  return data?.url || "";
}

import { Button, Drawer, Input, Space, Tag, message } from "antd";
import { useEffect, useState } from "react";
import { apiResponseError } from "../../apiError";
import type { PlaygroundOperation } from "./types";

export default function RequestDrawer({ operation, model, body, onSend }: { operation?: PlaygroundOperation; model: string; body: Record<string, unknown>; onSend: (raw: string) => void }) {
  const [open, setOpen] = useState(false);
  const [raw, setRaw] = useState("");
  const presetKey = `playground.presets.${model}.${operation?.type || ""}`;
  useEffect(() => { if (open) setRaw(JSON.stringify(body, null, 2)); }, [body, open]);
  if (!operation || !model) return null;
  const savePreset = () => { localStorage.setItem(presetKey, raw); message.success("Preset saved"); };
  const loadPreset = () => { const value = localStorage.getItem(presetKey); if (value) setRaw(value); else message.info("No saved preset"); };
  const skeleton = async () => {
    const route = operation.discovery?.find((item) => item.endpoint === "/openapi.json");
    if (!route) return;
    try {
      const response = await fetch(`/api/v1/playground/openapi.json?model=${encodeURIComponent(model)}`, { credentials: "include" });
      if (!response.ok) throw await apiResponseError(response, "OpenAPI discovery failed");
      const schema = await response.json() as any;
      const declared = schema.paths?.[operation.endpoint]?.post?.requestBody?.content?.["application/json"]?.schema;
      const content = resolveOpenAPISchema(schema, declared);
      const skeleton: Record<string, unknown> = { ...body };
      for (const name of content?.required || []) {
        if (skeleton[name] === undefined) skeleton[name] = name === "model" ? model : "";
      }
      skeleton.model = model;
      setRaw(JSON.stringify(skeleton, null, 2));
    } catch (error) { message.error((error as Error).message); }
  };
  return <><Button onClick={() => setOpen(true)} aria-label="Request">Request</Button><Drawer open={open} title="Request" width={680} onClose={() => setOpen(false)} extra={<Space><Button onClick={skeleton}>OpenAPI skeleton</Button><Button onClick={savePreset}>Save preset</Button><Button onClick={loadPreset}>Load preset</Button><Button type="primary" onClick={() => { onSend(raw); setOpen(false); }}>Send Request</Button></Space>}>
    <Space direction="vertical" style={{ width: "100%" }}><Space><Tag>{operation.mode === "realtime" ? "GET" : "POST"}</Tag><code>{operation.endpoint}</code></Space><Input.TextArea aria-label="Raw request body" value={raw} onChange={(event) => setRaw(event.target.value)} autoSize={{ minRows: 16 }} /></Space>
  </Drawer></>;
}

function resolveOpenAPISchema(document: any, schema: any): any {
  if (!schema?.$ref || typeof schema.$ref !== "string" || !schema.$ref.startsWith("#/")) return schema;
  return schema.$ref.slice(2).split("/").reduce((value: any, token: string) => value?.[token.split("~1").join("/").split("~0").join("~")], document);
}

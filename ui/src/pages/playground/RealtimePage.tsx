import UnsupportedModel from "./UnsupportedModel"
import RequestDrawer from "./RequestDrawer";
import { Alert, Button, Empty, Input, Select, Space, Tag, Typography, message } from "antd";
import { useEffect, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { modelsForOperation, operationOf } from "./operations";
import type { PlaygroundCatalog } from "./types";
import { useTranslation } from "react-i18next";

export default function RealtimePage({ catalog }: { catalog: PlaygroundCatalog }) {
  const { t } = useTranslation("playground");
  const [searchParams, setSearchParams] = useSearchParams();
  const models = modelsForOperation(catalog.models, "openai.realtime");
  const requested = searchParams.get("model") || "";
  const model = requested || models[0]?.id || "";
  const operation = operationOf(models.find((item) => item.id === model), "openai.realtime");
  const [state, setState] = useState<"disconnected" | "connecting" | "connected">("disconnected");
  const defaultEvent = JSON.stringify({ type: "session.update", model, session: {} });
  const [event, setEvent] = useState(defaultEvent);
  const [events, setEvents] = useState<string[]>([]);
  const socketRef = useRef<WebSocket>();

  useEffect(() => {
    const next = new URLSearchParams(searchParams);
    next.set("operation", "openai.realtime");
    if (model) next.set("model", model); else next.delete("model");
    if (next.toString() !== searchParams.toString()) setSearchParams(next, { replace: true });
  }, [model, searchParams, setSearchParams]);
  useEffect(() => setEvent(defaultEvent), [defaultEvent]);
  useEffect(() => () => socketRef.current?.close(), []);

  const connect = () => {
    if (!operation || !model || socketRef.current) return;
    const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
    const url = `${protocol}//${window.location.host}/api/v1/playground${operation.endpoint}?model=${encodeURIComponent(model)}`;
    setState("connecting");
    setEvents([]);
    const socket = new WebSocket(url);
    socketRef.current = socket;
    socket.onopen = () => { setState("connected"); setEvents((current) => [...current, t("realtime.connected")]); };
    socket.onmessage = (incoming) => setEvents((current) => [...current, typeof incoming.data === "string" ? incoming.data : t("realtime.binaryEvent")]);
    socket.onerror = () => setEvents((current) => [...current, t("realtime.connectionError")]);
    socket.onclose = () => { socketRef.current = undefined; setState("disconnected"); setEvents((current) => [...current, t("realtime.disconnected")]); };
  };
  const disconnect = () => socketRef.current?.close();
  const send = () => {
    if (socketRef.current?.readyState !== WebSocket.OPEN) return;
    try {
      const normalized = JSON.stringify(JSON.parse(event));
      socketRef.current.send(normalized);
      setEvents((current) => [...current, `> ${normalized}`]);
    } catch (error) {
      void message.error(t("realtime.invalidJSON", { detail: (error as Error).message }));
    }
  };

  if (requested && !operation) return <UnsupportedModel modelID={requested} models={catalog.models} />;
  if (!models.length) return <Empty description={t("realtime.noModels")} />;
  return <div className="playground-realtime-page">
    <Space direction="vertical" size="middle" style={{ width: "100%" }}>
      <Space wrap>
        <RequestDrawer operation={operation} model={model} body={{ type: "session.update", model, session: {} }} onSend={(raw) => { setEvent(raw); if (socketRef.current?.readyState === WebSocket.OPEN) socketRef.current.send(raw); }} /><Select value={model} aria-label={t("realtime.modelPlaceholder")} className="playground-model-select" options={models.map((item) => ({ value: item.id, label: item.label }))} onChange={(value) => { const next = new URLSearchParams(searchParams); next.set("model", value); setSearchParams(next, { replace: true }); }} />
        <Tag color={state === "connected" ? "green" : state === "connecting" ? "blue" : "default"}>{t(`realtime.${state}`)}</Tag>
        {state === "disconnected" ? <Button type="primary" onClick={connect}>{t("realtime.connect")}</Button> : <Button danger onClick={disconnect}>{t("realtime.disconnect")}</Button>}
      </Space>
      {models.find((item) => item.id === model)?.sleeping && <Alert type="warning" showIcon message={t("realtime.sleeping")} />}
      <Typography.Title level={5}>{t("realtime.outgoingEvent")}</Typography.Title>
      <Input.TextArea aria-label={t("realtime.outgoingEvent")} value={event} onChange={(change) => setEvent(change.target.value)} autoSize={{ minRows: 4 }} />
      <Button disabled={state !== "connected"} onClick={send}>{t("realtime.send")}</Button>
      <Typography.Title level={5}>{t("realtime.eventLog")}</Typography.Title>
      <pre className="playground-realtime-log">{events.join("\n") || t("realtime.empty")}</pre>
    </Space>
  </div>;
}

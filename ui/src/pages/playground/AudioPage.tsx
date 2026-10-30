import UnsupportedModel from "./UnsupportedModel";
import InputsForm from "./InputsForm";
import { composeRequest, declaredValues } from "./compose";
import { useDiscovery } from "./useDiscovery";
import RequestDrawer from "./RequestDrawer";
import { AudioOutlined, DeleteOutlined, PauseOutlined, PlayCircleOutlined, UploadOutlined } from "@ant-design/icons";
import { Alert, Button, Empty, Input, Segmented, Select, Space, Spin, Typography, Upload, message } from "antd";
import { useEffect, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { invokeJSON, invokeMultipart } from "./api";
import { modelsForOperation, operationOf } from "./operations";
import type { PlaygroundCatalog } from "./types";
import { useTranslation } from "react-i18next";
import { formatBytes } from "../../format";

export default function AudioPage({ catalog }: { catalog: PlaygroundCatalog }) {
  const { t } = useTranslation("playground");
  const [searchParams, setSearchParams] = useSearchParams();
  const mode = searchParams.get("operation") === "openai.audio.speech" ? "speech" : "transcribe";
  const setMode = (value: "transcribe" | "speech") => {
    const next = new URLSearchParams(searchParams);
    next.set("operation", value === "speech" ? "openai.audio.speech" : "openai.audio.transcribe");
    setSearchParams(next, { replace: true });
  };
  const operationType = mode === "speech" ? "openai.audio.speech" as const : "openai.audio.transcribe" as const;
  const models = modelsForOperation(catalog.models, operationType);
  const requested = searchParams.get("model") || "";
  const model = requested || models[0]?.id || "";
  const operation = operationOf(models.find((item) => item.id === model), operationType);
  const [file, setFile] = useState<File>();
  const voices = useDiscovery<{ voices?: string[] }>(models.find((item) => item.id === model), operation?.discovery?.find((route) => route.name === "voices"));
  const declaredInputs = (operation?.inputs || []).map((field) => field.name === "voice" && voices.data?.voices?.length ? { ...field, options: voices.data.voices } : field);
  const [recording, setRecording] = useState(false);
  const [requestingMicrophone, setRequestingMicrophone] = useState(false);
  const recordingUnavailable = window.isSecureContext === false
    ? "audio.recordInsecure"
    : typeof navigator.mediaDevices?.getUserMedia !== "function" || typeof MediaRecorder !== "function"
      ? "audio.recordUnsupported" : undefined;
  const [recordedSeconds, setRecordedSeconds] = useState(0);
  const [loading, setLoading] = useState(false);
  const [result, setResult] = useState<unknown>();
  const [elapsedMS, setElapsedMS] = useState<number>();
  const [speechText, setSpeechText] = useState("Hello from GPUWorks");
  const [inputValues, setInputValues] = useState<Record<string, unknown>>({});
  const declaredInputValues = operation ? declaredValues(operation, inputValues) : {};
  const [rawRequest, setRawRequest] = useState<string>();
  const [audioURL, setAudioURL] = useState("");
  const mountedRef = useRef(true);
  const recorderRef = useRef<MediaRecorder>();
  const streamRef = useRef<MediaStream>();
  const timerRef = useRef<number>();
  const abortRef = useRef<AbortController>();

  // Discovery is asynchronous.  A CustomVoice operation is unusable until
  // the voice list arrives, so select the first advertised voice as the
  // page's default.  Keep the initial render alive while the discovery
  // request is in flight; composeRequest intentionally rejects incomplete
  // requests, while the request drawer still needs a draft to render.
  useEffect(() => {
    if (!operation?.inputs?.some((field) => field.name === "voice")) return;
    const firstVoice = voices.data?.voices?.[0];
    if (!firstVoice) return;
    setInputValues((current) => current.voice === undefined ? { ...current, voice: firstVoice } : current);
  }, [operation, voices.data]);

  useEffect(() => {
    const next = new URLSearchParams(searchParams);
    next.set("operation", operationType);
    if (model) next.set("model", model); else next.delete("model");
    if (next.toString() !== searchParams.toString()) setSearchParams(next, { replace: true });
  }, [model, operationType, searchParams, setSearchParams]);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      const recorder = recorderRef.current;
      if (recorder) {
        recorder.onstop = null;
        recorder.ondataavailable = null;
        if (recorder.state !== "inactive") recorder.stop();
      }
      streamRef.current?.getTracks().forEach((track) => track.stop());
      if (timerRef.current) window.clearInterval(timerRef.current);
      abortRef.current?.abort();
    };
  }, []);
  useEffect(() => () => { if (audioURL) URL.revokeObjectURL(audioURL); }, [audioURL]);

  const selectModel = (value: string) => {
    const next = new URLSearchParams(searchParams);
    next.set("model", value);
    setSearchParams(next, { replace: true });
  };
  const startRecording = async () => {
    if (recordingUnavailable || requestingMicrophone) return;
    setRequestingMicrophone(true);
    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
      if (!mountedRef.current) {
        stream.getTracks().forEach((track) => track.stop());
        return;
      }
      streamRef.current = stream;
      const chunks: Blob[] = [];
      const recorder = new MediaRecorder(stream);
      recorder.ondataavailable = (event) => { if (event.data.size) chunks.push(event.data); };
      recorder.onstop = () => {
        const mime = recorder.mimeType || "audio/webm";
        setFile(new File(chunks, `recording-${Date.now()}.webm`, { type: mime }));
        setResult(undefined);
        setElapsedMS(undefined);
        stream.getTracks().forEach((track) => track.stop());
        streamRef.current = undefined;
        recorderRef.current = undefined;
      };
      recorderRef.current = recorder;
      setRecordedSeconds(0);
      setRecording(true);
      recorder.start(250);
      timerRef.current = window.setInterval(() => setRecordedSeconds((value) => value + 1), 1000);
    } catch (error) {
      const recorder = recorderRef.current;
      if (recorder) {
        recorder.onstop = null;
        recorder.ondataavailable = null;
        if (recorder.state !== "inactive") recorder.stop();
      }
      streamRef.current?.getTracks().forEach((track) => track.stop());
      streamRef.current = undefined;
      recorderRef.current = undefined;
      if (timerRef.current) window.clearInterval(timerRef.current);
      if (mountedRef.current) {
        setRecording(false);
        message.error(t("audio.recordFailed", { detail: (error as Error).message }));
      }
    } finally {
      if (mountedRef.current) setRequestingMicrophone(false);
    }
  };
  const stopRecording = () => {
    recorderRef.current?.stop();
    if (timerRef.current) window.clearInterval(timerRef.current);
    setRecording(false);
  };
  const submit = async (rawOverride?: string) => {
    if (!operation || !model || (mode === "transcribe" && !file) || (mode === "speech" && !speechText.trim())) return;
    const controller = new AbortController();
    abortRef.current = controller;
    setLoading(true);
    setResult(undefined);
    setElapsedMS(undefined);
    const started = performance.now();
    try {
      const value = mode === "transcribe"
        ? await invokeMultipart(operation, model, {}, file, controller.signal)
        : await invokeJSON(operation, model, rawOverride || rawRequest ? JSON.parse(rawOverride || rawRequest!) : composeRequest(operation, model, { input: speechText }, declaredInputValues), controller.signal);
      // Blob can come from a different realm (for example, the fetch
      // implementation in a test runner or an embedded webview).  An
      // instanceof check would reject an otherwise valid binary response.
      if (isBlobLike(value)) {
        if (audioURL) URL.revokeObjectURL(audioURL);
        setAudioURL(URL.createObjectURL(value));
        setResult({ audio: true, bytes: value.size });
      } else {
        setResult(value);
      }
      setElapsedMS(performance.now() - started);
    } catch (error) {
      if ((error as Error).name !== "AbortError") message.error((error as Error).message);
    } finally {
      setLoading(false);
      abortRef.current = undefined;
    }
  };
  const text = typeof result === "object" && result && "text" in result ? String((result as { text: unknown }).text) : result && !audioURL ? JSON.stringify(result, null, 2) : "";

  return (
    <div className="playground-audio-page">
      <section className="playground-audio-workbench">
        <div className="playground-audio-toolbar">
          <Segmented value={mode} options={[{ value: "transcribe", label: t("audio.transcribe") }, { value: "speech", label: t("audio.speech") }]} onChange={(value) => { setMode(value as "transcribe" | "speech"); setResult(undefined); setAudioURL(""); }} />
          <Select
            value={model || undefined}
            placeholder={t("audio.modelPlaceholder")}
            onChange={selectModel}
            options={models.map((item) => ({ value: item.id, label: item.label }))}
            className="playground-model-select"
          />
          {mode === "speech" && <RequestDrawer operation={operation} model={model} body={composeDraft(operation || { type: operationType, endpoint: "/v1/audio/speech", mode: "sync", request_encodings: ["json"], renderer: "audio", response: "audio" }, model, { input: speechText }, declaredInputValues)} onSend={(raw) => { setRawRequest(raw); void submit(raw); }} />}
          {model && catalog.models.find((item) => item.id === model)?.sleeping && <Alert type="warning" showIcon message={t("audio.sleeping")} />}
        </div>
        {requested && !operation ? <UnsupportedModel modelID={requested} models={catalog.models} /> : !models.length ? (
          <Empty description={t("audio.noModels")} />
        ) : (
          mode === "speech" ? <div className="playground-audio-input"><Typography.Title level={5}>{t("audio.speechInput")}</Typography.Title><Input.TextArea value={speechText} onChange={(event) => setSpeechText(event.target.value)} autoSize={{ minRows: 5 }} />{declaredInputs.length > 0 && <InputsForm fields={declaredInputs} values={inputValues} onChange={(name, value) => setInputValues((current) => ({ ...current, [name]: value }))} />}<Button type="primary" size="large" disabled={!speechText.trim() || !model} loading={loading} onClick={() => void submit()}>{t("audio.synthesize")}</Button>{loading && <div className="playground-transcribing"><Spin /><Button size="small" onClick={() => abortRef.current?.abort()}>{t("audio.stop")}</Button></div>}</div> : <div className="playground-audio-input">
            <div className={`playground-recorder${recording ? " is-recording" : ""}`}>
              <div className="playground-recorder-icon"><AudioOutlined /></div>
              <Typography.Title level={5}>{recording ? formatDuration(recordedSeconds) : file?.name || t("audio.input")}</Typography.Title>
              <Typography.Text type="secondary">{file ? formatFileSize(file.size) : t("audio.formats")}</Typography.Text>
              <Space wrap>
                <Upload beforeUpload={(next) => { setFile(next); setResult(undefined); setElapsedMS(undefined); return false; }} showUploadList={false} accept="audio/*">
                  <Button icon={<UploadOutlined />}>{t("audio.select")}</Button>
                </Upload>
                {recording ? (
                  <Button danger icon={<PauseOutlined />} onClick={stopRecording}>{t("audio.stopRecording")}</Button>
                ) : (
                  <Button icon={<PlayCircleOutlined />} disabled={Boolean(recordingUnavailable)} loading={requestingMicrophone} onClick={startRecording}>{t("audio.startRecording")}</Button>
                )}
                {file && <Button type="text" danger icon={<DeleteOutlined />} onClick={() => { setFile(undefined); setResult(undefined); setElapsedMS(undefined); }}>{t("audio.remove")}</Button>}
              </Space>
            </div>
            {recordingUnavailable && <Alert type="warning" showIcon message={t(recordingUnavailable)} />}
            <Button type="primary" size="large" disabled={!file || !model} loading={loading} onClick={() => void submit()}>
              {t("audio.start")}
            </Button>
            {loading && <div className="playground-transcribing"><Spin /><Typography.Text>{t("audio.transcribing")}</Typography.Text><Button size="small" onClick={() => abortRef.current?.abort()}>{t("audio.stop")}</Button></div>}
          </div>
        )}
      </section>
      <section className="playground-transcript">
        <div className="playground-transcript-header">
          <Typography.Title level={5}>{t("audio.result")}</Typography.Title>
          {elapsedMS !== undefined && <Typography.Text type="secondary">{t("audio.elapsed", { seconds: (elapsedMS / 1000).toFixed(2) })}</Typography.Text>}
        </div>
        {audioURL ? <audio src={audioURL} controls /> : text ? <Typography.Paragraph copyable className="playground-transcript-text">{text}</Typography.Paragraph> : <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t("audio.empty")} />}
      </section>
    </div>
  );
}

function isBlobLike(value: unknown): value is Blob {
  return !!value && typeof value === "object" && typeof (value as Blob).size === "number" && typeof (value as Blob).arrayBuffer === "function";
}

function composeDraft(operation: Parameters<typeof composeRequest>[0], model: string, primary: Record<string, unknown>, values: Record<string, unknown>): Record<string, unknown> {
  try {
    return composeRequest(operation, model, primary, values);
  } catch {
    const body: Record<string, unknown> = { ...primary, ...(operation.request_defaults || {}) };
    for (const field of operation.inputs || []) if (field.default !== undefined && body[field.name] === undefined) body[field.name] = field.default;
    Object.assign(body, values);
    body.model = model;
    return body;
  }
}

function formatDuration(seconds: number): string {
  return `${String(Math.floor(seconds / 60)).padStart(2, "0")}:${String(seconds % 60).padStart(2, "0")}`;
}

const formatFileSize = (bytes: number) => formatBytes(bytes, { minUnit: "KiB", maxUnit: "MiB" });

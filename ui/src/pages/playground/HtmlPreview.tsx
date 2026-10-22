import { DownloadOutlined, EyeOutlined, FullscreenExitOutlined, FullscreenOutlined, ReloadOutlined } from "@ant-design/icons";
import { Alert, Button, Drawer, Modal, Segmented, Space, Tag, Tooltip, Typography } from "antd";
import { createContext, forwardRef, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import CopyButton from "../../components/CopyButton";
import { useIsAdmin } from "../../components/useIsAdmin";
import { blockedTarget, buildPreviewDocument, linkToOpen, parsePreviewMessage, PREVIEW_SANDBOX } from "./previewSandbox";

/** What the drawer shows: one block of one reply, kept current while it streams. */
interface PreviewState {
  key: string;
  language: string;
  code: string;
  /** The block's closing fence arrived, or its reply finished. */
  complete: boolean;
}

interface PreviewController {
  activeKey?: string;
  open: (state: PreviewState) => void;
  update: (state: PreviewState) => void;
  detach: (key: string) => void;
}

const HtmlPreviewContext = createContext<PreviewController | undefined>(undefined);

/**
 * HtmlPreviewProvider owns the page's single preview drawer. Preview buttons
 * anywhere below it open their block in it; the open block's button keeps the
 * drawer's copy of the code current, and closes the drawer when its reply
 * goes away (cleared, another model selected, its column removed).
 */
export function HtmlPreviewProvider({ allowNetwork, children }: { allowNetwork: boolean; children: ReactNode }) {
  const [state, setState] = useState<PreviewState>();
  const open = useCallback((next: PreviewState) => setState(next), []);
  const update = useCallback((next: PreviewState) => setState((current) => (
    current?.key === next.key && (current.code !== next.code || current.complete !== next.complete) ? next : current
  )), []);
  const detach = useCallback((key: string) => setState((current) => current?.key === key ? undefined : current), []);
  const controller = useMemo(() => ({ activeKey: state?.key, open, update, detach }), [state?.key, open, update, detach]);
  return (
    <HtmlPreviewContext.Provider value={controller}>
      {children}
      {state && <HtmlPreviewDrawer state={state} allowNetwork={allowNetwork} onClose={() => setState(undefined)} />}
    </HtmlPreviewContext.Provider>
  );
}

/** PreviewAction is the eye button on a previewable block or reply. */
export function PreviewAction({ previewKey, language, code, complete }: { previewKey: string; language: string; code: string; complete: boolean }) {
  const { t } = useTranslation("playground");
  const preview = useContext(HtmlPreviewContext);
  const active = preview?.activeKey === previewKey;
  const update = preview?.update;
  const detach = preview?.detach;
  useEffect(() => {
    if (active) update?.({ key: previewKey, language, code, complete });
  }, [active, update, previewKey, language, code, complete]);
  useEffect(() => () => detach?.(previewKey), [detach, previewKey]);
  if (!preview) return null;
  return (
    <Tooltip title={t("preview.open")}>
      <Button size="small" type="text" icon={<EyeOutlined />} aria-label={t("preview.open")} onClick={() => preview.open({ key: previewKey, language, code, complete })} />
    </Tooltip>
  );
}

function hash(text: string): string {
  let value = 5381;
  for (let index = 0; index < text.length; index++) value = (value * 33) ^ text.charCodeAt(index);
  return (value >>> 0).toString(36);
}

function HtmlPreviewDrawer({ state, allowNetwork, onClose }: { state: PreviewState; allowNetwork: boolean; onClose: () => void }) {
  const { t } = useTranslation("playground");
  const isAdmin = useIsAdmin();
  const [modal, modalHolder] = Modal.useModal();
  const [view, setView] = useState<"preview" | "source">("preview");
  const [fullscreen, setFullscreen] = useState(false);
  const [nonce, setNonce] = useState(0);
  const [blocked, setBlocked] = useState<string[]>([]);
  const [navigated, setNavigated] = useState(false);
  const frameRef = useRef<HTMLIFrameElement>(null);
  const sourceRef = useRef<HTMLPreElement>(null);
  // Rendering half a document reruns its scripts and replays its animations
  // on every chunk, so the frame waits for the block to finish.
  const srcdoc = useMemo(() => state.complete ? buildPreviewDocument(state.code, { allowNetwork }) : undefined, [state.complete, state.code, allowNetwork]);
  const showFrame = view === "preview" && srcdoc !== undefined;

  useEffect(() => {
    setBlocked([]);
    setNavigated(false);
  }, [srcdoc]);
  useEffect(() => {
    if (!state.complete && sourceRef.current) sourceRef.current.scrollTop = sourceRef.current.scrollHeight;
  }, [state.code, state.complete, showFrame]);

  useEffect(() => {
    const onMessage = (event: MessageEvent) => {
      if (!frameRef.current || event.source !== frameRef.current.contentWindow) return;
      const message = parsePreviewMessage(event.data);
      if (message?.type === "blocked") {
        const target = blockedTarget(message.uri);
        setBlocked((current) => current.includes(target) ? current : [...current, target]);
      } else if (message?.type === "open") {
        const url = linkToOpen(message.url);
        if (!url) return;
        if (allowNetwork) {
          window.open(url, "_blank", "noopener,noreferrer");
          return;
        }
        modal.confirm({
          title: t("preview.openLinkTitle"),
          content: <Typography.Text code style={{ wordBreak: "break-all" }}>{url}</Typography.Text>,
          okText: t("preview.openLink"),
          cancelText: t("preview.cancel"),
          onOk: () => { window.open(url, "_blank", "noopener,noreferrer"); },
        });
      }
    };
    window.addEventListener("message", onMessage);
    return () => window.removeEventListener("message", onMessage);
  }, [allowNetwork, modal, t]);

  // A script can navigate the frame itself (location = ...), which neither the
  // sandbox nor CSP stops. Under the restrictive policy, put the preview back.
  const onNavigate = useCallback(() => {
    if (allowNetwork) return;
    setNavigated(true);
    setNonce((value) => value + 1);
  }, [allowNetwork]);

  const reload = () => {
    setBlocked([]);
    setNavigated(false);
    setNonce((value) => value + 1);
  };
  const download = () => {
    const url = URL.createObjectURL(new Blob([state.code], { type: state.language === "svg" ? "image/svg+xml" : "text/html;charset=utf-8" }));
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = state.language === "svg" ? "preview.svg" : "preview.html";
    anchor.click();
    setTimeout(() => URL.revokeObjectURL(url), 0);
  };

  return (
    <Drawer
      open
      title={t("preview.title")}
      placement="right"
      width={fullscreen ? "100%" : "min(960px, 100vw)"}
      onClose={onClose}
      className="playground-preview-drawer"
      extra={(
        <Space size={4} wrap>
          <Segmented
            size="small"
            value={view}
            onChange={(value) => setView(value as "preview" | "source")}
            options={[{ value: "preview", label: t("preview.preview") }, { value: "source", label: t("preview.source") }]}
          />
          <Tooltip title={t("preview.reload")}><Button size="small" type="text" icon={<ReloadOutlined />} aria-label={t("preview.reload")} disabled={!srcdoc} onClick={reload} /></Tooltip>
          <CopyButton size="small" label={t("preview.copySource")} text={state.code} />
          <Tooltip title={t("preview.download")}><Button size="small" type="text" icon={<DownloadOutlined />} aria-label={t("preview.download")} onClick={download} /></Tooltip>
          <Tooltip title={fullscreen ? t("preview.exitFullscreen") : t("preview.fullscreen")}>
            <Button size="small" type="text" icon={fullscreen ? <FullscreenExitOutlined /> : <FullscreenOutlined />} aria-label={fullscreen ? t("preview.exitFullscreen") : t("preview.fullscreen")} onClick={() => setFullscreen((value) => !value)} />
          </Tooltip>
        </Space>
      )}
    >
      {modalHolder}
      <div className="playground-preview-body">
        <Typography.Text type="secondary" className="playground-preview-note">{allowNetwork ? t("preview.noteOpen") : t("preview.noteRestricted")}</Typography.Text>
        {blocked.length > 0 && (
          <Alert
            type="warning"
            showIcon
            message={t("preview.blocked", { targets: blocked.join(", ") })}
            description={isAdmin ? <Link to="/settings#security">{t("preview.openSettings")}</Link> : t("preview.askAdmin")}
          />
        )}
        {navigated && <Alert type="info" showIcon message={t("preview.navigated")} />}
        {!state.complete && <Space size={6}><Tag color="processing">{t("preview.generating")}</Tag><Typography.Text type="secondary">{t("preview.waiting")}</Typography.Text></Space>}
        {showFrame
          ? <PreviewFrame key={`${nonce}-${hash(srcdoc)}`} ref={frameRef} srcdoc={srcdoc} title={t("preview.frameTitle")} onNavigate={onNavigate} />
          : <pre ref={sourceRef} className="playground-preview-source">{state.code}</pre>}
      </div>
    </Drawer>
  );
}

interface PreviewFrameProps {
  srcdoc: string;
  title: string;
  onNavigate: () => void;
}

/**
 * PreviewFrame is keyed by its document, so a fresh document is a fresh
 * element: every load after its first is the page navigating itself away.
 */
const PreviewFrame = forwardRef<HTMLIFrameElement, PreviewFrameProps>(function PreviewFrame({ srcdoc, title, onNavigate }, ref) {
  const loads = useRef(0);
  return (
    <iframe
      ref={ref}
      className="playground-preview-frame"
      title={title}
      sandbox={PREVIEW_SANDBOX}
      srcDoc={srcdoc}
      onLoad={() => {
        loads.current += 1;
        if (loads.current > 1) onNavigate();
      }}
    />
  );
});

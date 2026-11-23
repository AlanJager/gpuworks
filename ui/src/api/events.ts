import { openChangeStream } from "../events/changeStream";
import { useEffect, useRef } from "react";
import { apiResponseError } from "../apiError";
import { isSessionExpired, subscribeSessionChanges } from "../session";

// All hooks share one EventSource. Opening one connection per hook exhausts the
// browser's per-origin HTTP/1.1 connection pool and leaves ordinary API
// refreshes pending behind the long-lived SSE streams.
let sharedEventSource: ReturnType<typeof openChangeStream> | null = null;
let sharedEventSubscribers = 0;
let eventSessionCheck: Promise<void> | null = null;

function acquireEventSource(): ReturnType<typeof openChangeStream> {
  if (!sharedEventSource) {
    sharedEventSource = openChangeStream(response => {
      if (response) { void apiResponseError(response, ""); return; }
      // EventSource hides HTTP statuses, so distinguish authentication expiry
      // from an ordinary connection failure through a management request.
      if (eventSessionCheck) return;
      eventSessionCheck = fetch("/api/v1/auth/me", { credentials: "include" })
        .then(async (response) => { if (!response.ok) await apiResponseError(response, ""); })
        .catch(() => { /* A network failure does not establish session expiry. */ })
        .finally(() => { eventSessionCheck = null; });
    });
  }
  sharedEventSubscribers += 1;
  return sharedEventSource;
}

function releaseEventSource(source: ReturnType<typeof openChangeStream>) {
  sharedEventSubscribers = Math.max(0, sharedEventSubscribers - 1);
  if (sharedEventSubscribers === 0 && sharedEventSource === source) {
    source.close();
    sharedEventSource = null;
  }
}

// useEvents subscribes to the shared SSE stream and invokes onChange whenever
// a matching topic event arrives (events carry no payload; callers refetch).
export function useEvents(topic: string, onChange: () => void) {
  const cb = useRef(onChange);
  cb.current = onChange;
  useEffect(() => {
    const handler = () => cb.current();
    return subscribeEvents(topic, handler, handler);
  }, [topic]);
}

export interface ChangeEvent {
  key?: string;
  reason?: string;
}

export function useEventPayload(
  topic: string,
  onEvent: (event: ChangeEvent) => void,
) {
  const cb = useRef(onEvent);
  cb.current = onEvent;
  useEffect(() => {
    const handler = (raw: Event) => {
      try {
        cb.current(
          JSON.parse((raw as MessageEvent<string>).data) as ChangeEvent,
        );
      } catch {
        cb.current({});
      }
    };
    return subscribeEvents(topic, handler, () => cb.current({ reason: "session-restored" }));
  }, [topic]);
}

function subscribeEvents(topic: string, handler: EventListener, restored: () => void): () => void {
  let source: ReturnType<typeof openChangeStream> | null = null;
  const stop = () => {
    if (!source) return;
    source.removeEventListener(topic, handler);
    releaseEventSource(source);
    source = null;
  };
  const start = () => {
    if (source) return;
    source = acquireEventSource();
    source.addEventListener(topic, handler);
  };
  if (!isSessionExpired()) start();
  const unsubscribe = subscribeSessionChanges((change) => {
    if (change === "expired") stop();
    else { start(); restored(); }
  });
  return () => { unsubscribe(); stop(); };
}

// One native, cookie-authenticated EventSource per origin / session generation.
// Subscribe to topic names supplied by pages, so new server events need no registry here.
const pages = new Map<MessagePort, Set<string>>();
const handlers = new Map<string, EventListener>();
let source: EventSource | null = null;
let reconnect: ReturnType<typeof setTimeout> | undefined;

const initialRetry = 3_000;
let retryDelay = initialRetry;
const pendingRefresh = new Set<MessagePort>();
let checkingSession = false;

function refresh(port: MessagePort, topic: string) {
  port.postMessage({ topic, data: '{"reason":"resync"}' });
}

// One credential check per shared connection failure, even with many tabs.
// Only propagate the status and product error code, never account details.
async function checkSession() {
  if (checkingSession) return;
  checkingSession = true;
  try {
    const response = await fetch("/api/v1/auth/me", { credentials: "include" });
    if (response.ok) return;
    const { code } = await response.json();
    if (typeof code === "string") {
      for (const port of pages.keys()) port.postMessage({ error: true, status: response.status, code });
    }
  } catch { /* A transport failure cannot establish session expiry. */ }
  finally { checkingSession = false; }
}

function syncTopics() {
  const topics = new Set([...pages.values()].flatMap(names => [...names]));
  if (!topics.size) {
    clearTimeout(reconnect);
    reconnect = undefined;
    retryDelay = initialRetry;
    source?.close();
    source = null;
    handlers.clear();
    return;
  }
  if (reconnect !== undefined) return;
  if (!source) {
    source = new EventSource("/api/v1/events");
    const current = source;
    current.onopen = () => {
      if (source !== current) return;
      retryDelay = initialRetry;
      for (const port of pendingRefresh) {
        // The server already sends the initial node event on every connection.
        for (const topic of pages.get(port) ?? []) if (topic !== "node") refresh(port, topic);
      }
      pendingRefresh.clear();
    };
    source.onerror = () => {
      if (source !== current) return;
      for (const port of pages.keys()) pendingRefresh.add(port);
      void checkSession();
      // CONNECTING streams retry natively. CLOSED streams (e.g. an HTTP 502)
      // will never reopen, even when another tab joins this named worker.
      if (current.readyState === EventSource.CLOSED) {
        current.close();
        source = null;
        handlers.clear();
        const delay = retryDelay * (0.8 + 0.2 * Math.random());
        retryDelay = Math.min(30_000, retryDelay * 2);
        reconnect = setTimeout(() => {
          reconnect = undefined;
          syncTopics();
        }, delay);
      }
    };
  }
  for (const [topic, handler] of handlers) {
    if (topics.has(topic)) continue;
    source.removeEventListener(topic, handler);
    handlers.delete(topic);
  }
  for (const topic of topics) {
    if (handlers.has(topic)) continue;
    const handler = (event: Event) => {
      for (const [port, interests] of pages) {
        if (interests.has(topic)) port.postMessage({ topic, data: (event as MessageEvent<string>).data });
      }
    };
    handlers.set(topic, handler);
    source.addEventListener(topic, handler);
  }
}

self.addEventListener("connect", (event: Event) => {
  const port = (event as MessageEvent).ports[0];
  let expiry: ReturnType<typeof setTimeout> | undefined;
  let expired = false;
  const forget = () => {
    pendingRefresh.delete(port);
    clearTimeout(expiry);
    if (pages.delete(port)) syncTopics();
  };
  port.onmessage = ({ data }) => {
    if (data.close) { forget(); port.close(); return; }
    // A crashed page cannot send close. Live pages renew every 30 seconds;
    // allow background timer throttling and reattach if a frozen page resumes.
    clearTimeout(expiry);
    const wasOpen = source?.readyState === EventSource.OPEN;
    const hadNode = pages.get(port)?.has("node");
    const names = new Set<string>(data.topics);
    if (!names.size) { forget(); return; }
    pages.set(port, names);
    expiry = setTimeout(() => { expired = true; forget(); }, 180_000);
    const resumed = expired || data.resume;
    if (resumed && !wasOpen) pendingRefresh.add(port);
    syncTopics();
    // A new page joining an already open stream also gets the server's initial node refresh.
    if (resumed && wasOpen) {
      for (const topic of names) refresh(port, topic);
    } else if (!hadNode && wasOpen && names.has("node")) port.postMessage({ topic: "node", data: '{"reason":"connected"}' });
    expired = false;
  };
});
export {};

// One native, cookie-authenticated EventSource per origin / session generation.
// Subscribe to topic names supplied by pages, so new server events need no registry here.
const pages = new Map<MessagePort, Set<string>>();
const handlers = new Map<string, EventListener>();
let source: EventSource | null = null;
let reconnect: ReturnType<typeof setTimeout> | undefined;

function syncTopics() {
  const topics = new Set([...pages.values()].flatMap(names => [...names]));
  if (!topics.size) {
    clearTimeout(reconnect);
    reconnect = undefined;
    source?.close();
    source = null;
    handlers.clear();
    return;
  }
  if (reconnect !== undefined) return;
  if (!source) {
    source = new EventSource("/api/v1/events");
    const current = source;
    source.onerror = () => {
      if (source !== current) return;
      for (const port of pages.keys()) port.postMessage({ error: true });
      // CONNECTING streams retry natively. CLOSED streams (e.g. an HTTP 502)
      // will never reopen, even when another tab joins this named worker.
      if (current.readyState === EventSource.CLOSED) {
        current.close();
        source = null;
        handlers.clear();
        reconnect = setTimeout(() => {
          reconnect = undefined;
          syncTopics();
        }, 3_000);
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
  const forget = () => {
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
    expiry = setTimeout(forget, 180_000);
    syncTopics();
    // A new page joining an already open stream also gets the server's initial node refresh.
    if (!hadNode && wasOpen && names.has("node")) port.postMessage({ topic: "node", data: '{"reason":"connected"}' });
  };
});
export {};

import { localizedResponseError } from "../http";
import { ConnectionTestInterrupted, type ConnectionCheckEvent, type ConnectionTestSummary } from "../connectionTest";
import { isInferenceCatalogPackage } from "../inferenceTemplates";

export async function getText(path: string, signal?: AbortSignal): Promise<string> {
  const res = await fetch(path, { signal, credentials: "include" });
  if (!res.ok)
    throw await localizedResponseError(res, `${path}: ${res.status}`);
  return res.text();
}

export async function sendFile<T>(path: string, file: File): Promise<T> {
  const res = await fetch(path, {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": isInferenceCatalogPackage(file) ? "application/gzip" : "application/yaml" },
    body: file,
  });
  if (!res.ok) {
    throw await localizedResponseError(res, `${path}: ${res.status}`);
  }
  return res.json() as Promise<T>;
}


export function eventPageQuery(page?: { limit?: number; before?: number }): string {
  const params = new URLSearchParams();
  if (page?.limit) params.set("limit", String(page.limit));
  if (page?.before) params.set("before", String(page.before));
  const query = params.toString();
  return query ? `?${query}` : "";
}

const isAbort = (error: unknown) => (error as { name?: unknown } | null)?.name === "AbortError";

// postSSE consumes a POSTed text/event-stream response. EventSource cannot be
// used here: it only issues GET, and these endpoints carry credentials in the
// request body. An absent body is sent as no body at all. Aborting the signal
// ends the request and rejects with the AbortError; any other break of the
// connection rejects with ConnectionTestInterrupted.
async function postSSE(
  path: string,
  body: unknown,
  onEvent: (event: string, data: unknown) => void,
  signal?: AbortSignal,
): Promise<void> {
  const interrupted = (error: unknown) => (isAbort(error) ? error : new ConnectionTestInterrupted());
  const res = await fetch(path, {
    method: "POST",
    credentials: "include",
    signal,
    ...(body === undefined ? {} : { headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) }),
  }).catch((error) => { throw interrupted(error); });
  if (!res.ok || !res.body) {
    throw await localizedResponseError(res, `${path}: ${res.status}`);
  }
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  for (;;) {
    const { done, value } = await reader.read().catch((error) => { throw interrupted(error); });
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    let idx: number;
    while ((idx = buf.indexOf("\n\n")) >= 0) {
      const frame = buf.slice(0, idx);
      buf = buf.slice(idx + 2);
      let event = "message";
      let data = "";
      for (const line of frame.split("\n")) {
        if (line.startsWith("event: ")) event = line.slice(7);
        else if (line.startsWith("data: ")) data += line.slice(6);
      }
      if (data) onEvent(event, JSON.parse(data));
    }
  }
}

export function streamConnectionTest(
  path: string,
  body: unknown,
  onCheck: (c: ConnectionCheckEvent) => void,
  signal?: AbortSignal,
): Promise<ConnectionTestSummary> {
  return new Promise((resolve, reject) => {
    let settled = false;
    postSSE(path, body, (event, data) => {
      if (event === "check") onCheck(data as ConnectionCheckEvent);
      else if (event === "done") {
        settled = true;
        resolve(data as ConnectionTestSummary);
      }
    }, signal)
      .then(() => {
        // A clean EOF without a done frame means the stream was truncated
        // before the summary; surface it instead of hanging the dialog.
        if (!settled) reject(new ConnectionTestInterrupted());
      })
      .catch(reject);
  });
}

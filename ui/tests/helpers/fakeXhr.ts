// A hand-driven XMLHttpRequest for upload tests. It keeps the parts of the
// real object's contract the upload code relies on: setRequestHeader and send
// throw InvalidStateError before open (and send twice), abort fires onabort
// only while the request is in flight and nothing after it has finished, and
// response headers are looked up case-insensitively, null when absent.

type Handler = ((event?: unknown) => void) | null;

export class FakeXHR {
  static instances: FakeXHR[] = [];
  static readonly UNSENT = 0;
  static readonly OPENED = 1;
  static readonly DONE = 4;

  readyState = 0;
  method = "";
  url = "";
  withCredentials = false;
  requestHeaders: Record<string, string> = {};
  body: unknown = undefined;
  status = 0;
  statusText = "";
  responseText = "";
  aborted = false;
  private responseHeaders: Record<string, string> = {};
  private sent = false;
  upload: { onprogress: ((event: { loaded: number; total: number; lengthComputable: boolean }) => void) | null } = { onprogress: null };
  onload: Handler = null;
  onerror: Handler = null;
  onabort: Handler = null;

  open(method: string, url: string) {
    this.method = method;
    this.url = url;
    this.readyState = FakeXHR.OPENED;
  }

  setRequestHeader(name: string, value: string) {
    if (this.readyState !== FakeXHR.OPENED || this.sent) throw new DOMException("setRequestHeader before open", "InvalidStateError");
    this.requestHeaders[name] = value;
  }

  send(body?: unknown) {
    if (this.readyState !== FakeXHR.OPENED || this.sent) throw new DOMException("send before open", "InvalidStateError");
    this.sent = true;
    this.body = body;
    FakeXHR.instances.push(this);
  }

  abort() {
    if (!this.sent || this.readyState === FakeXHR.DONE) return;
    this.aborted = true;
    this.readyState = FakeXHR.DONE;
    this.status = 0;
    this.onabort?.();
  }

  getResponseHeader(name: string): string | null {
    const found = Object.entries(this.responseHeaders).find(([key]) => key.toLowerCase() === name.toLowerCase());
    return found ? found[1] : null;
  }

  /** The browser reports that loaded bytes of the body went out. */
  progress(loaded: number, total: number, lengthComputable = true) {
    this.upload.onprogress?.({ loaded, total: lengthComputable ? total : 0, lengthComputable });
  }

  /** The server answers. */
  respond(status: number, body: string, headers: Record<string, string> = { "Content-Type": "application/json" }, statusText = "") {
    if (this.readyState === FakeXHR.DONE) throw new Error("request already finished");
    this.status = status;
    this.statusText = statusText;
    this.responseText = body;
    this.responseHeaders = headers;
    this.readyState = FakeXHR.DONE;
    this.onload?.();
  }

  /** The connection breaks: no status, no body. */
  fail() {
    this.readyState = FakeXHR.DONE;
    this.status = 0;
    this.onerror?.();
  }
}

/** Puts FakeXHR in place of XMLHttpRequest; the returned function puts back what was there. */
export function installFakeXHR(): () => void {
  const had = Object.getOwnPropertyDescriptor(globalThis, "XMLHttpRequest");
  FakeXHR.instances = [];
  Object.defineProperty(globalThis, "XMLHttpRequest", { value: FakeXHR, configurable: true, writable: true });
  return () => {
    if (had) Object.defineProperty(globalThis, "XMLHttpRequest", had);
    else delete (globalThis as { XMLHttpRequest?: unknown }).XMLHttpRequest;
    FakeXHR.instances = [];
  };
}

/** The latest request sent, once one is. */
export function lastXHR(): FakeXHR {
  const xhr = FakeXHR.instances.at(-1);
  if (!xhr) throw new Error("no upload request sent");
  return xhr;
}

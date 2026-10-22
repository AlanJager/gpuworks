import { useEffect, useState } from "react";
import { api } from "../../api";

// The trace link template is one platform setting; every request detail on a
// page shares one read of it. A failed read is "no link": the trace id is
// still shown and copyable.
let pending: Promise<string> | null = null;

function loadTraceLink(): Promise<string> {
  pending ??= api
    .telemetryExport()
    .then((exp) => exp.trace_link)
    .catch(() => {
      pending = null; // try again next time
      return "";
    });
  return pending;
}

// resetTraceLinkCache forgets the template (after the setting changes; tests).
export function resetTraceLinkCache() {
  pending = null;
}

// useTraceLink is the trace link template, "" until known or when none is set.
export function useTraceLink(enabled = true): string {
  const [link, setLink] = useState("");
  useEffect(() => {
    if (!enabled) return;
    let live = true;
    void loadTraceLink().then((value) => {
      if (live) setLink(value);
    });
    return () => {
      live = false;
    };
  }, [enabled]);
  return link;
}

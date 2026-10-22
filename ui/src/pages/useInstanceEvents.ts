import { useCallback, useEffect, useState } from "react";

import { api, useEventPayload, type InstanceEvent } from "../api";

export const INSTANCE_EVENT_PAGE = 50;

// EventTimelineState used to also carry loading/loadingMore/loadMore for the
// standalone events tab (InstanceEventTimeline.tsx). That tab is gone -
// instance events now merge into the log stream as system lines - so the
// shape is trimmed to exactly what the log stream consumes: the events
// themselves, whether the last fetch failed (with a way to retry it), and
// whether the one page held is not the whole history.
export interface EventTimelineState {
  events: InstanceEvent[];
  error: boolean;
  hasMore: boolean;
  reload: () => void;
}

/**
 * useInstanceEvents owns one instance's timeline for the whole panel.
 *
 * There is a single fetch behind both the startup stage list and the log
 * stream's merged system lines: the panel used to pull the full history again
 * on every phase change just to recompute the stages. It reloads when the
 * shared SSE stream reports a change for this instance that is not merely
 * progress - progress moves byte counters and appends nothing, so refetching
 * on it would be pure waste.
 */
export function useInstanceEvents(uuid: string, enabled = true): EventTimelineState {
  const [events, setEvents] = useState<InstanceEvent[]>([]);
  const [error, setError] = useState(false);
  const [hasMore, setHasMore] = useState(false);
  const [reloads, setReloads] = useState(0);

  useEffect(() => {
    if (!enabled) return;
    let current = true;
    api.instanceEvents(uuid, { limit: INSTANCE_EVENT_PAGE })
      .then((page) => {
        if (!current) return;
        setEvents(page.items);
        setHasMore(page.items.length >= INSTANCE_EVENT_PAGE);
        setError(false);
      })
      .catch(() => { if (current) setError(true); });
    return () => { current = false; };
  }, [uuid, enabled, reloads]);

  const reload = useCallback(() => setReloads((value) => value + 1), []);

  useEventPayload("instance", (event) => {
    if (event.key !== uuid || event.reason === "progress") return;
    reload();
  });

  return { events, error, hasMore, reload };
}

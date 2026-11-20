import { useEffect, useState } from "react";

import { api, type InstanceEvent } from "../api";

const wakeOrderCodes = new Set(["AUTO_WAKE", "WAKE_ORDERED"]);

const namesInstance = (event: InstanceEvent, uuid: string) =>
  Array.isArray(event.params?.instances) && event.params.instances.includes(uuid);

const isAwakeResult = (event: InstanceEvent) =>
  event.code === "INSTANCE_SLEEP_RESULT" && event.params?.result === "applied" && event.params?.level === 0;

/**
 * lastWakeSeconds is how long the instance's latest recorded wake took: from
 * the order that named it (a request, a schedule or an operator) to the
 * engine reporting it awake. It is read from the two timelines the server
 * already keeps, newest first as the API returns them; a wake with no order
 * on record, or one whose result cannot be told from another replica's, has
 * no figure.
 *
 * The deployment's timeline holds the orders and every replica's results but
 * not whose result each is, so a result is this instance's when the order
 * woke it alone or when its own timeline holds the same entry.
 */
export function lastWakeSeconds(deploymentEvents: InstanceEvent[], instanceEvents: InstanceEvent[], uuid: string): number | undefined {
  const orderAt = deploymentEvents.findIndex((event) => wakeOrderCodes.has(event.code ?? "") && namesInstance(event, uuid));
  if (orderAt < 0) return undefined;
  const order = deploymentEvents[orderAt];
  const alone = (order.params?.instances as unknown[]).length === 1;
  const own = new Set(instanceEvents.filter(isAwakeResult).map((event) => event.id));
  // Entries after the order, oldest first, up to the next wake order: a
  // result past that one may answer it instead.
  for (let i = orderAt - 1; i >= 0; i--) {
    const event = deploymentEvents[i];
    if (wakeOrderCodes.has(event.code ?? "")) return undefined;
    if (!isAwakeResult(event)) continue;
    if (!alone && !own.has(event.id)) continue;
    const seconds = (Date.parse(event.created_at) - Date.parse(order.created_at)) / 1000;
    return Number.isFinite(seconds) && seconds >= 0 ? seconds : undefined;
  }
  return undefined;
}

/**
 * useLastWakeSeconds reads the deployment's timeline for lastWakeSeconds
 * while enabled, again whenever the instance's sleep level or order changes.
 */
export function useLastWakeSeconds(inst: { uuid: string; deployment_id: number; sleep_level?: number; desired_sleep?: number }, instanceEvents: InstanceEvent[], enabled: boolean): number | undefined {
  const [events, setEvents] = useState<InstanceEvent[]>([]);
  const sleepKey = `${inst.sleep_level ?? 0}:${inst.desired_sleep ?? 0}`;
  useEffect(() => {
    if (!enabled) return;
    let current = true;
    api.deploymentEvents(inst.deployment_id, { limit: 50 }).then((page) => page.items ?? [], () => [])
      .then((items) => { if (current) setEvents(items); });
    return () => { current = false; };
  }, [inst.deployment_id, sleepKey, enabled]);
  return enabled ? lastWakeSeconds(events, instanceEvents, inst.uuid) : undefined;
}

import { useCallback, useEffect, useRef, useState } from "react";
import { api, type Node } from "./api";

// The agent answers the upgrade RPC before it downloads, smoke-tests and
// execs the replacement binary, so the outcome has to be observed afterwards.
// The server does not record that an upgrade was requested; these snapshots
// taken at click time are what lets the page tell a new result from state
// that was already there. They are tracked per node on the page, not in the
// detail drawer, so closing the drawer or opening another node never loses
// or leaks them.

export type UpgradePhase = "requesting" | "waiting";

export interface PendingUpgrade {
  phase: UpgradePhase;
  name: string;
  startedAt: number;
  baselineSeen: number;
  baselineError?: string;
}

export type UpgradeOutcome = "pending" | "completed" | "failed" | "timeout";

export type UpgradeEvent =
  | { kind: "dispatched"; uuid: string; name: string }
  | { kind: "requestFailed"; uuid: string; name: string; detail: string }
  | { kind: "completed"; uuid: string; name: string; node: Node }
  | { kind: "failed"; uuid: string; name: string; detail: string }
  | { kind: "timeout"; uuid: string; name: string };

export const UPGRADE_TIMEOUT_MS = 2 * 60 * 1000;
export const UPGRADE_RECHECK_MS = 5000;

function seenAt(node: Node | undefined) {
  const parsed = node?.last_seen_at ? Date.parse(node.last_seen_at) : 0;
  return Number.isNaN(parsed) ? 0 : parsed;
}

export function evaluateUpgrade(
  pending: PendingUpgrade,
  node: Node | undefined,
  serverVersion: string | undefined,
  upgradeError: string | undefined,
  now: number,
): UpgradeOutcome {
  if (
    node &&
    serverVersion &&
    serverVersion !== "dev" &&
    node.agent_version === serverVersion &&
    node.state === "online" &&
    seenAt(node) > pending.baselineSeen
  ) {
    return "completed";
  }
  // The server clears an upgrade error only when the agent reconnects with
  // the server's version, so an error left over from an earlier attempt must
  // not fail a new one.
  if (upgradeError && upgradeError !== pending.baselineError) return "failed";
  if (now - pending.startedAt >= UPGRADE_TIMEOUT_MS) return "timeout";
  return "pending";
}

export function useAgentUpgrades({
  nodes,
  serverVersion,
  upgradeErrors,
  onEvent,
  upgradeNode = api.upgradeNode,
  now = Date.now,
}: {
  nodes: Node[] | null | undefined;
  serverVersion: string | undefined;
  upgradeErrors: Record<string, string> | null | undefined;
  onEvent: (event: UpgradeEvent) => void;
  upgradeNode?: (uuid: string) => Promise<unknown>;
  now?: () => number;
}) {
  const [pending, setPending] = useState<Record<string, PendingUpgrade>>({});
  const [tick, setTick] = useState(0);
  const latest = useRef({ nodes, upgradeErrors, onEvent, upgradeNode, now });
  latest.current = { nodes, upgradeErrors, onEvent, upgradeNode, now };
  // Tracks entries synchronously so a double click cannot send two requests
  // before the state update renders.
  const active = useRef(new Set<string>());

  const hasWaiting = Object.values(pending).some((p) => p.phase === "waiting");
  // Node list polling normally drives the evaluation; this keeps the timeout
  // firing even when that polling keeps failing and stops producing data.
  useEffect(() => {
    if (!hasWaiting) return;
    const id = window.setInterval(() => setTick((n) => n + 1), UPGRADE_RECHECK_MS);
    return () => window.clearInterval(id);
  }, [hasWaiting]);

  useEffect(() => {
    const at = latest.current.now();
    const settled: UpgradeEvent[] = [];
    for (const [uuid, p] of Object.entries(pending)) {
      if (p.phase !== "waiting") continue;
      const node = nodes?.find((n) => n.uuid === uuid);
      const error = upgradeErrors?.[uuid];
      const outcome = evaluateUpgrade(p, node, serverVersion, error, at);
      if (outcome === "completed" && node) {
        settled.push({ kind: "completed", uuid, name: p.name, node });
      } else if (outcome === "failed") {
        settled.push({ kind: "failed", uuid, name: p.name, detail: error ?? "" });
      } else if (outcome === "timeout") {
        settled.push({ kind: "timeout", uuid, name: p.name });
      }
    }
    if (settled.length === 0) return;
    for (const event of settled) active.current.delete(event.uuid);
    setPending((current) => {
      const next = { ...current };
      for (const event of settled) delete next[event.uuid];
      return next;
    });
    for (const event of settled) latest.current.onEvent(event);
  }, [pending, nodes, serverVersion, upgradeErrors, tick]);

  const start = useCallback(async (node: Node) => {
    const { nodes, upgradeErrors, upgradeNode, now } = latest.current;
    const uuid = node.uuid;
    // The drawer holds the node as it was when opened; the polled list is
    // fresher and gives a tighter last-seen baseline.
    const baseline = nodes?.find((n) => n.uuid === uuid) ?? node;
    const name = node.hostname || uuid;
    if (active.current.has(uuid)) return;
    active.current.add(uuid);
    setPending((current) => ({
      ...current,
      [uuid]: {
        phase: "requesting",
        name,
        startedAt: now(),
        baselineSeen: seenAt(baseline),
        baselineError: upgradeErrors?.[uuid],
      },
    }));
    try {
      await upgradeNode(uuid);
    } catch (e) {
      active.current.delete(uuid);
      setPending((current) => {
        const next = { ...current };
        delete next[uuid];
        return next;
      });
      latest.current.onEvent({
        kind: "requestFailed",
        uuid,
        name,
        detail: (e as Error).message,
      });
      return;
    }
    setPending((current) =>
      current[uuid]
        ? { ...current, [uuid]: { ...current[uuid], phase: "waiting" } }
        : current,
    );
    latest.current.onEvent({ kind: "dispatched", uuid, name });
  }, []);

  const phaseOf = useCallback(
    (uuid: string | undefined): UpgradePhase | undefined =>
      uuid ? pending[uuid]?.phase : undefined,
    [pending],
  );

  return { pending, start, phaseOf };
}

import { useSyncExternalStore } from "react";
import { useSessionUser } from "../components/sessionUser";

// A viewer's acknowledgements of console banners (dto.ConsoleBanner.ack_key).
// The server decides which banners can be acknowledged and under which key,
// and sends a new key when the matter should be raised again; the console
// only remembers, per signed-in user and in this browser, which keys that
// user acknowledged. The banners above every page and an overlay page that
// lists the same matter read this one store, so acknowledging in one place
// hides it in the other at once (and in the browser's other tabs).

const prefix = "gpuworks.banner-ack.";

/** bannerAckStorageKey is where the acknowledgement of ackKey by userID is kept. */
export function bannerAckStorageKey(userID: number, ackKey: string): string {
  return `${prefix}${userID}.${ackKey}`;
}

// Acknowledgements this visit could not store (no known user, storage
// refused): they still hold until the page reloads.
const unsaved = new Set<string>();
let version = 0;
const listeners = new Set<() => void>();

function bump() {
  version++;
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  // Another tab's acknowledgement arrives as a storage event; this tab's
  // own calls bump directly.
  if (listeners.size === 1) window.addEventListener("storage", onStorage);
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0) window.removeEventListener("storage", onStorage);
  };
}

function onStorage(event: StorageEvent) {
  if (event.key === null || event.key.startsWith(prefix)) bump();
}

const snapshot = () => version;

function stored(userID: number | undefined, ackKey: string): boolean {
  if (unsaved.has(`${userID ?? ""}.${ackKey}`)) return true;
  if (userID === undefined) return false;
  try { return window.localStorage.getItem(bannerAckStorageKey(userID, ackKey)) === "1"; }
  catch { return false; }
}

export interface BannerAck {
  /** acknowledged reports whether the viewer acknowledged ackKey; a banner without a key never is. */
  acknowledged: (ackKey: string | undefined) => boolean;
  /** acknowledge records the viewer's acknowledgement of ackKey. */
  acknowledge: (ackKey: string) => void;
}

/** useBannerAck reads and records the signed-in user's banner acknowledgements, re-rendering when any changes. */
export function useBannerAck(): BannerAck {
  const userID = useSessionUser()?.id;
  // Subscribing re-renders the caller whenever an acknowledgement changes, so
  // the functions below, made on each render, answer from the current store.
  // Neither is memoized: callers use them during render and in handlers, never
  // as effect or memo dependencies.
  useSyncExternalStore(subscribe, snapshot, snapshot);
  const acknowledged = (ackKey: string | undefined) => !!ackKey && stored(userID, ackKey);
  const acknowledge = (ackKey: string) => {
    let saved = false;
    if (userID !== undefined) {
      try { window.localStorage.setItem(bannerAckStorageKey(userID, ackKey), "1"); saved = true; }
      catch { /* kept for this visit below */ }
    }
    if (!saved) unsaved.add(`${userID ?? ""}.${ackKey}`);
    bump();
  };
  return { acknowledged, acknowledge };
}

/** resetBannerAckForTest forgets the acknowledgements kept only in memory. */
export function resetBannerAckForTest() {
  unsaved.clear();
  bump();
}

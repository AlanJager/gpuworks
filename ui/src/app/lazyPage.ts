import { lazy, type ComponentType } from "react";

const reloadedKey = "gpuworks.chunkReload";

/**
 * lazyPage loads a page as its own chunk. A chunk that cannot be fetched has
 * usually been replaced by a server upgrade while this tab stayed open: the
 * console is loaded again once, and a page that still fails goes to the error
 * boundary. reload is the window's own unless a test passes one.
 */
export function lazyPage(load: () => Promise<{ default: ComponentType }>, reload: () => void = () => window.location.reload()) {
  return lazy(() => load().then(
    (module) => { forget(); return module; },
    (error: unknown) => {
      // Without a mark that survives the reload, a second failure could not
      // be told from the first and would reload for ever.
      if (remembered() === window.location.pathname || !remember(window.location.pathname)) throw error;
      reload();
      // The reload replaces this document; nothing renders in the meantime.
      return new Promise<{ default: ComponentType }>(() => {});
    },
  ));
}

// Session storage may be blocked; then every failure goes to the error boundary's reload button.
function remembered() {
  try { return sessionStorage.getItem(reloadedKey); } catch { return window.location.pathname; }
}
function remember(path: string) {
  try { sessionStorage.setItem(reloadedKey, path); return true; } catch { return false; }
}
function forget() {
  try { sessionStorage.removeItem(reloadedKey); } catch { /* blocked */ }
}

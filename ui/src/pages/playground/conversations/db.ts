import type { Conversation } from "./model";

const DB_NAME = "gpuworks-playground";
const STORE = "conversations";
const BY_USER = "userID";

/** ConversationDB is where the browser keeps an account's conversations. */
export interface ConversationDB {
  list(userID: number): Promise<Conversation[]>;
  get(id: string): Promise<Conversation | undefined>;
  put(conversation: Conversation): Promise<void>;
  remove(ids: string[]): Promise<void>;
}

function request<T>(req: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

function done(tx: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
    tx.onabort = () => reject(tx.error ?? new Error("IndexedDB transaction aborted"));
  });
}

/**
 * openConversationDB opens the IndexedDB store: one record per conversation,
 * keyed by id and indexed by the account it belongs to. It rejects where
 * IndexedDB is missing or refused (some private windows), and the caller
 * then keeps conversations in memory only.
 */
export async function openConversationDB(factory: IDBFactory | undefined = globalThis.indexedDB): Promise<ConversationDB> {
  if (!factory) throw new Error("IndexedDB is not available");
  const open = factory.open(DB_NAME, 1);
  open.onupgradeneeded = () => {
    const store = open.result.createObjectStore(STORE, { keyPath: "id" });
    store.createIndex(BY_USER, BY_USER);
  };
  const db = await request(open);
  // Another tab upgrading the schema must not be blocked by this one.
  db.onversionchange = () => db.close();
  return {
    async list(userID) {
      return request(db.transaction(STORE).objectStore(STORE).index(BY_USER).getAll(userID)) as Promise<Conversation[]>;
    },
    async get(id) {
      return request(db.transaction(STORE).objectStore(STORE).get(id)) as Promise<Conversation | undefined>;
    },
    async put(conversation) {
      const tx = db.transaction(STORE, "readwrite");
      tx.objectStore(STORE).put(conversation);
      await done(tx);
    },
    async remove(ids) {
      if (!ids.length) return;
      const tx = db.transaction(STORE, "readwrite");
      for (const id of ids) tx.objectStore(STORE).delete(id);
      await done(tx);
    },
  };
}

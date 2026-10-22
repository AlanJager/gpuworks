import type { ChatMessage, ParameterState } from "../types";
import { newColumn, newConversation, titleFrom, type Conversation, type ConversationColumn } from "./model";

/**
 * SyncMessage is what pages of the same browser tell each other about the
 * conversation store: a record changed or was deleted, and the probe a page
 * sends to learn whether the pages that left replies streaming still run.
 */
export type SyncMessage =
  | { type: "put"; userID: number; id: string }
  | { type: "delete"; userID: number; ids: string[] }
  | { type: "probe"; userID: number; owners: string[]; nonce: string }
  | { type: "alive"; userID: number; owner: string; nonce: string };

export interface PeerChannel {
  post(message: SyncMessage): void;
  listen(handler: (message: SyncMessage) => void): void;
  close(): void;
}

const CHANNEL = "gpuworks-playground";

/** broadcastPeers links the pages of this browser; without BroadcastChannel a page is alone. */
export function broadcastPeers(): PeerChannel {
  if (typeof BroadcastChannel === "undefined") return { post() {}, listen() {}, close() {} };
  const channel = new BroadcastChannel(CHANNEL);
  return {
    post: (message) => channel.postMessage(message),
    listen: (handler) => { channel.onmessage = (event: MessageEvent<SyncMessage>) => handler(event.data); },
    close: () => channel.close(),
  };
}

// Before conversations moved to IndexedDB each chat column kept a session in
// sessionStorage under these keys, one history per model.
const LEGACY_KEYS = ["gpuworks.playground.single.0", "gpuworks.playground.compare.0", "gpuworks.playground.compare.1", "gpuworks.playground.compare.2"];

interface LegacyPaneSession {
  version: 1;
  modelID?: string;
  parameters?: ParameterState;
  conversations?: Record<string, ChatMessage[]>;
}

function readLegacy(storage: Storage, key: string): LegacyPaneSession | undefined {
  try {
    const session = JSON.parse(storage.getItem(key) || "null") as LegacyPaneSession | null;
    return session?.version === 1 ? session : undefined;
  } catch {
    return undefined;
  }
}

function legacyColumn(model: string, parameters: ParameterState, messages: ChatMessage[]): ConversationColumn {
  return {
    ...newColumn(model, parameters),
    messages: messages.map((message) => ({ ...message, streaming: false, ...(message.role === "assistant" ? { model } : {}) })),
  };
}

function titled(conversation: Conversation): Conversation {
  const first = conversation.columns[0]?.messages.find((message) => message.role === "user");
  return { ...conversation, title: titleFrom(first?.content || "", first?.attachments?.[0]?.name) };
}

/**
 * takeLegacySessions turns the sessionStorage sessions of the old playground
 * into conversations and removes them: every model with history in the
 * single column becomes a single conversation, and the compare columns
 * together become one compare conversation.
 */
export function takeLegacySessions(storage: Storage, userID: number, now: number): Conversation[] {
  const sessions = LEGACY_KEYS.map((key) => readLegacy(storage, key));
  const out: Conversation[] = [];
  const [single, ...compare] = sessions;
  const parametersOf = (session: LegacyPaneSession) => session.parameters ?? { enabled: {}, values: {}, systemPrompt: "", extraJSON: "" };
  for (const [model, messages] of Object.entries(single?.conversations ?? {})) {
    if (!messages.length) continue;
    out.push(titled(newConversation(userID, "single", [legacyColumn(model, parametersOf(single!), messages)], now)));
  }
  const columns = compare
    .filter((session): session is LegacyPaneSession => !!session?.modelID)
    .map((session) => legacyColumn(session.modelID!, parametersOf(session), session.conversations?.[session.modelID!] ?? []));
  if (columns.length >= 2 && columns.some((column) => column.messages.length)) out.push(titled(newConversation(userID, "compare", columns, now)));
  for (const key of LEGACY_KEYS) storage.removeItem(key);
  return out;
}

/** PartialReply is the text a reply had when its page went away. */
export interface PartialReply {
  conversationID: string;
  messageID: string;
  content: string;
  reasoning?: string;
}

// IndexedDB writes started while a page unloads may never finish, and saves
// run at most once a second. sessionStorage is written synchronously and
// survives a reload of the tab, so the page leaves its partial replies there
// and the reloaded page merges them back.
const UNLOAD_KEY = "gpuworks.playground.unload";

export function keepPartialReplies(storage: Storage, userID: number, replies: PartialReply[]) {
  try {
    if (replies.length) storage.setItem(`${UNLOAD_KEY}.${userID}`, JSON.stringify(replies));
  } catch {
    // Full or blocked storage: the last periodic save is what remains.
  }
}

export function takePartialReplies(storage: Storage, userID: number): PartialReply[] {
  const key = `${UNLOAD_KEY}.${userID}`;
  try {
    const raw = JSON.parse(storage.getItem(key) || "[]") as unknown;
    storage.removeItem(key);
    return Array.isArray(raw) ? raw.filter((reply): reply is PartialReply => typeof reply?.messageID === "string" && typeof reply?.content === "string") : [];
  } catch {
    storage.removeItem(key);
    return [];
  }
}

/** mergePartialReplies puts the text a reply reached back into the reply the record still has as streaming. */
export function mergePartialReplies(conversation: Conversation, replies: PartialReply[]): Conversation {
  const byMessage = new Map(replies.filter((reply) => reply.conversationID === conversation.id).map((reply) => [reply.messageID, reply]));
  if (!byMessage.size) return conversation;
  return {
    ...conversation,
    columns: conversation.columns.map((column) => ({
      ...column,
      messages: column.messages.map((message) => {
        const reply = byMessage.get(message.id);
        if (!reply || !message.streaming || reply.content.length < message.content.length) return message;
        return { ...message, content: reply.content, reasoning: reply.reasoning ?? message.reasoning };
      }),
    })),
  };
}

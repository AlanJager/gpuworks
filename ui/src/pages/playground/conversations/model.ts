import type { ChatMessage, ParameterState } from "../types";

// MAX_CONVERSATIONS is how many conversations one account keeps in this
// browser; older ones are dropped when a newer one is saved.
export const MAX_CONVERSATIONS = 60;
export const MAX_COLUMNS = 3;
export const TITLE_LENGTH = 40;

export type ConversationMode = "single" | "compare";

export interface ConversationColumn {
  id: string;
  model: string;
  parameters: ParameterState;
  messages: ChatMessage[];
}

/**
 * Conversation is one record of the browser's playground store, read and
 * written whole. A compare conversation has two or three columns that share
 * the composer; each column keeps its own history.
 */
export interface Conversation {
  id: string;
  userID: number;
  // Empty until the first question names it.
  title: string;
  titleEdited: boolean;
  createdAt: number;
  updatedAt: number;
  mode: ConversationMode;
  columns: ConversationColumn[];
}

export function newID(prefix: string): string {
  return `${prefix}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

export function newColumn(model: string, parameters: ParameterState): ConversationColumn {
  return { id: newID("column"), model, parameters: structuredClone(parameters), messages: [] };
}

export function newConversation(userID: number, mode: ConversationMode, columns: ConversationColumn[], now: number): Conversation {
  return { id: newID("conversation"), userID, title: "", titleEdited: false, createdAt: now, updatedAt: now, mode, columns };
}

export function isEmpty(conversation: Conversation): boolean {
  return conversation.columns.every((column) => !column.messages.length);
}

/** inherit starts an empty conversation with the models and parameters of `from`. */
export function inherit(from: Conversation, mode: ConversationMode, now: number): Conversation {
  const columns = mode === from.mode
    ? from.columns.map((column) => newColumn(column.model, column.parameters))
    : mode === "compare"
      ? [newColumn(from.columns[0].model, from.columns[0].parameters), newColumn(from.columns[0].model, from.columns[0].parameters)]
      : [newColumn(from.columns[0].model, from.columns[0].parameters)];
  return newConversation(from.userID, mode, columns, now);
}

export function titleFrom(text: string, attachmentName?: string): string {
  const line = text.trim().replace(/\s+/g, " ");
  const source = line || attachmentName || "";
  return source.length > TITLE_LENGTH ? `${source.slice(0, TITLE_LENGTH)}…` : source;
}

/** streamingMessages lists the replies the record says are still being generated. */
export function streamingMessages(conversation: Conversation): ChatMessage[] {
  return conversation.columns.flatMap((column) => column.messages.filter((message) => message.streaming));
}

/**
 * markInterrupted turns replies left streaming by a page that is gone into
 * interrupted ones. Pages that are still generating keep theirs.
 */
export function markInterrupted(conversation: Conversation, liveOwners: Set<string>): Conversation {
  let changed = false;
  const columns = conversation.columns.map((column) => ({
    ...column,
    messages: column.messages.map((message) => {
      if (!message.streaming || (message.owner && liveOwners.has(message.owner))) return message;
      changed = true;
      return { ...message, streaming: false, owner: undefined, interrupted: true };
    }),
  }));
  return changed ? { ...conversation, columns } : conversation;
}

/** newestFirst orders conversations by their last change. */
export function newestFirst(a: Conversation, b: Conversation): number {
  return b.updatedAt - a.updatedAt;
}

/**
 * overflow returns the ids beyond the MAX_CONVERSATIONS newest. Conversations
 * still generating and the protected ids are never dropped.
 */
export function overflow(conversations: Conversation[], protectedIDs: Set<string>, limit = MAX_CONVERSATIONS): string[] {
  const ordered = [...conversations].sort(newestFirst);
  const drop: string[] = [];
  let kept = 0;
  for (const conversation of ordered) {
    if (kept < limit || protectedIDs.has(conversation.id) || streamingMessages(conversation).length) {
      kept += 1;
      continue;
    }
    drop.push(conversation.id);
  }
  return drop;
}

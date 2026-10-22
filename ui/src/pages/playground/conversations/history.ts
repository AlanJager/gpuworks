import { userContent } from "../api";
import type { CompletionMessage } from "../types";
import type { ConversationColumn } from "./model";

/**
 * historyMessages is the column's history as the next request sends it, after
 * its system prompt. Failed, stopped and interrupted replies are left out.
 */
export function historyMessages(column: ConversationColumn): CompletionMessage[] {
  const out: CompletionMessage[] = [];
  if (column.parameters.systemPrompt.trim()) out.push({ role: "system", content: column.parameters.systemPrompt.trim() });
  for (const message of column.messages) {
    if (message.error || message.interrupted || message.streaming) continue;
    out.push({ role: message.role, content: message.role === "user" ? userContent(message.content, message.attachments || []) : message.content });
  }
  return out;
}

import { buildChatBody } from "../api";
import { streamLimit, type StreamLimit } from "../streamLimit";
import type { Attachment, ChatMessage, ParameterState, StreamOptionDefinition } from "../types";
import { openConversationDB, type ConversationDB } from "./db";
import { historyMessages } from "./history";
import {
  inherit, isEmpty, markInterrupted, MAX_COLUMNS, newColumn, newConversation, newestFirst, newID, overflow, streamingMessages, titleFrom,
  type Conversation, type ConversationColumn, type ConversationMode,
} from "./model";
import { Streams } from "./streams";
import { broadcastPeers, keepPartialReplies, mergePartialReplies, takeLegacySessions, takePartialReplies, type PeerChannel, type SyncMessage } from "./sync";

export { ABORTED_ERROR } from "./streams";
export { historyMessages } from "./history";

export interface ConversationState {
  status: "idle" | "loading" | "ready";
  userID?: number;
  // False when IndexedDB could not be opened: conversations live in memory only.
  persistent: boolean;
  // Newest first.
  conversations: Conversation[];
  currentID?: string;
  // Replies this page is generating, against limit.limit.
  running: number;
  limit: StreamLimit;
}

export type SendResult =
  | { status: "sent" }
  | { status: "limited"; need: number; running: number; limit: number }
  | { status: "invalid"; error: string }
  | { status: "busy" };

export interface ColumnDefaults {
  model: string;
  parameters: ParameterState;
}

export interface StoreOptions {
  openDB?: () => Promise<ConversationDB>;
  peers?: () => PeerChannel;
  session?: Storage;
  now?: () => number;
  clock?: () => number;
  limit?: () => StreamLimit;
  // How long a probe waits for the page that left a reply streaming.
  probeWaitMS?: number;
  // How often a streaming conversation is saved.
  saveIntervalMS?: number;
  pageID?: string;
}

const CURRENT_KEY = "gpuworks.playground.current";

function sessionStorageOrUndefined(): Storage | undefined {
  try {
    return globalThis.sessionStorage;
  } catch {
    return undefined;
  }
}

/**
 * ConversationStore owns the playground conversations of the signed-in
 * account and the replies being generated. It lives outside the React tree,
 * so leaving the page, switching playground modes or switching between
 * single and compare never stops a reply. Conversations are kept in
 * IndexedDB; the pages of one browser keep each other's lists current
 * through a BroadcastChannel.
 */
export class ConversationStore {
  private state: ConversationState;
  private readonly listeners = new Set<() => void>();
  private readonly streams: Streams;
  private readonly saveTimers = new Map<string, ReturnType<typeof setTimeout>>();
  private readonly persisted = new Set<string>();
  private readonly probes = new Map<string, Set<string>>();
  private readonly opts: Required<Omit<StoreOptions, "session">> & { session?: Storage };
  private db?: ConversationDB;
  private peers?: PeerChannel;
  private loading?: Promise<void>;
  private generation = 0;

  constructor(options: StoreOptions = {}) {
    this.opts = {
      openDB: options.openDB ?? (() => openConversationDB()),
      peers: options.peers ?? broadcastPeers,
      session: options.session ?? sessionStorageOrUndefined(),
      now: options.now ?? Date.now,
      clock: options.clock ?? (() => performance.now()),
      limit: options.limit ?? (() => streamLimit()),
      probeWaitMS: options.probeWaitMS ?? 3000,
      saveIntervalMS: options.saveIntervalMS ?? 1000,
      pageID: options.pageID ?? newID("page"),
    };
    this.state = { status: "idle", persistent: true, conversations: [], running: 0, limit: this.opts.limit() };
    this.streams = new Streams({
      clock: this.opts.clock,
      patch: (conversationID, messageID, patch, save) => this.patchMessage(conversationID, messageID, patch, save),
      changed: (running) => this.streamsChanged(running),
    });
    if (typeof window !== "undefined") {
      window.addEventListener("pagehide", this.onPageHide);
    }
  }

  readonly subscribe = (listener: () => void) => {
    this.listeners.add(listener);
    return () => { this.listeners.delete(listener); };
  };

  readonly getState = () => this.state;

  get pageID() { return this.opts.pageID; }

  current(): Conversation | undefined {
    return this.state.conversations.find((conversation) => conversation.id === this.state.currentID);
  }

  /** start loads the account's conversations; calling it again for the same account is a no-op. */
  start(userID: number): Promise<void> {
    if (this.state.userID === userID && this.loading) return this.loading;
    this.reset();
    const generation = this.generation;
    this.set({ status: "loading", userID });
    this.loading = this.load(userID, generation);
    return this.loading;
  }

  /** signOut stops every reply and forgets the account's conversations in this page. */
  signOut() {
    this.reset();
    this.set({ status: "idle", userID: undefined, conversations: [], currentID: undefined, persistent: true });
  }

  dispose() {
    this.reset();
    if (typeof window !== "undefined") window.removeEventListener("pagehide", this.onPageHide);
  }

  /**
   * ensureConversation opens a conversation when none is open: an empty one
   * with the given model and parameters.
   */
  ensureConversation(defaults: ColumnDefaults) {
    if (this.state.status !== "ready" || this.current()) return;
    const fresh = newConversation(this.state.userID!, "single", [newColumn(defaults.model, defaults.parameters)], this.opts.now());
    this.set({ conversations: [fresh, ...this.state.conversations], currentID: fresh.id });
    this.rememberCurrent();
  }

  /** newConversation opens an empty conversation with the current one's models and parameters; an empty current one is reused. */
  newConversation(mode?: ConversationMode) {
    const from = this.current();
    if (!from) return;
    if (isEmpty(from) && (!mode || mode === from.mode)) return;
    const fresh = inherit(from, mode ?? from.mode, this.opts.now());
    this.set({ conversations: [fresh, ...this.state.conversations], currentID: fresh.id });
    this.rememberCurrent();
  }

  /** openModel shows the given model: in the current conversation when it is empty, otherwise in a new single conversation. */
  openModel(model: string) {
    const from = this.current();
    if (!from || from.columns[0].model === model) return;
    if (isEmpty(from)) {
      this.setModel(from.columns[0].id, model);
      return;
    }
    const fresh = newConversation(from.userID, "single", [newColumn(model, from.columns[0].parameters)], this.opts.now());
    this.set({ conversations: [fresh, ...this.state.conversations], currentID: fresh.id });
    this.rememberCurrent();
  }

  open(id: string) {
    if (!this.state.conversations.some((conversation) => conversation.id === id)) return;
    // An empty conversation that was never saved is dropped when left.
    const left = this.current();
    const conversations = left && left.id !== id && isEmpty(left) && !this.persisted.has(left.id)
      ? this.state.conversations.filter((conversation) => conversation.id !== left.id)
      : this.state.conversations;
    this.set({ conversations, currentID: id });
    this.rememberCurrent();
  }

  rename(id: string, title: string) {
    const trimmed = title.trim();
    if (!trimmed) return;
    this.update(id, (conversation) => ({ ...conversation, title: trimmed, titleEdited: true }), "now");
  }

  remove(id: string) {
    this.streams.abortConversations(new Set([id]));
    this.clearSaveTimer(id);
    const conversations = this.state.conversations.filter((conversation) => conversation.id !== id);
    const currentID = this.state.currentID === id ? conversations[0]?.id : this.state.currentID;
    this.set({ conversations, currentID });
    this.rememberCurrent();
    if (this.persisted.delete(id)) void this.removeStored([id]);
  }

  /**
   * setMode switches the current conversation between single and compare.
   * An empty conversation switches in place; one with messages stays as it
   * is and a new conversation opens in the other mode.
   */
  setMode(mode: ConversationMode) {
    const from = this.current();
    if (!from || from.mode === mode) return;
    if (!isEmpty(from)) {
      this.newConversation(mode);
      return;
    }
    const switched = inherit(from, mode, this.opts.now());
    this.update(from.id, (conversation) => ({ ...conversation, mode, columns: switched.columns }), "none");
  }

  addColumn() {
    const from = this.current();
    if (!from || from.mode !== "compare" || from.columns.length >= MAX_COLUMNS) return;
    const last = from.columns[from.columns.length - 1];
    this.update(from.id, (conversation) => ({ ...conversation, columns: [...conversation.columns, newColumn(last.model, last.parameters)] }), "throttled");
  }

  /** removeColumn drops the last compare column and its history. */
  removeColumn() {
    const from = this.current();
    if (!from || from.mode !== "compare" || from.columns.length <= 2) return;
    const last = from.columns[from.columns.length - 1];
    this.abortColumn(from.id, last);
    this.update(from.id, (conversation) => ({ ...conversation, columns: conversation.columns.slice(0, -1) }), "throttled");
  }

  /** setModel answers the column's next question with another model; the history stays. */
  setModel(columnID: string, model: string) {
    this.updateColumn(columnID, (column, conversation) => {
      this.abortColumn(conversation.id, column);
      return { ...column, model };
    });
  }

  setParameters(columnID: string, parameters: ParameterState) {
    this.updateColumn(columnID, (column) => ({ ...column, parameters }));
  }

  applyParameters(fromColumnID: string) {
    const from = this.current();
    const source = from?.columns.find((column) => column.id === fromColumnID);
    if (!from || !source) return;
    this.update(from.id, (conversation) => ({
      ...conversation,
      columns: conversation.columns.map((column) => column.id === source.id ? column : { ...column, parameters: structuredClone(source.parameters) }),
    }), "throttled");
  }

  /** clear empties one column, or every column of the current conversation. */
  clear(columnID?: string) {
    const from = this.current();
    if (!from) return;
    for (const column of from.columns) if (!columnID || column.id === columnID) this.abortColumn(from.id, column);
    this.update(from.id, (conversation) => ({
      ...conversation,
      columns: conversation.columns.map((column) => !columnID || column.id === columnID ? { ...column, messages: [] } : column),
    }), "now");
  }

  stop(columnID: string) {
    const from = this.current();
    const column = from?.columns.find((candidate) => candidate.id === columnID);
    if (from && column) this.abortColumn(from.id, column);
  }

  /** generating reports whether this page is generating a reply in the column. */
  generating(column: ConversationColumn): boolean {
    return column.messages.some((message) => message.streaming && message.owner === this.opts.pageID);
  }

  /**
   * send asks every column of the current conversation the same question.
   * It is refused as a whole when the columns would take the page past its
   * stream limit, or when another page is still generating here.
   */
  send(text: string, attachments: Attachment[], streamOptions: StreamOptionDefinition[]): SendResult {
    const conversation = this.current();
    if (!conversation || (!text.trim() && !attachments.length)) return { status: "invalid", error: "" };
    if (streamingMessages(conversation).some((message) => message.owner !== this.opts.pageID)) return { status: "busy" };
    const own = this.streams.countIn(conversation.id);
    const need = conversation.columns.length;
    const { limit } = this.state.limit;
    if (this.streams.size - own + need > limit) return { status: "limited", need, running: this.streams.size, limit };
    try {
      for (const column of conversation.columns) buildChatBody(column.model, historyMessages(column), column.parameters, streamOptions);
    } catch (error) {
      return { status: "invalid", error: (error as Error).message };
    }
    for (const column of conversation.columns) this.abortColumn(conversation.id, column);
    const now = this.opts.now();
    const started: { column: ConversationColumn; message: ChatMessage }[] = [];
    this.update(conversation.id, (current) => ({
      ...current,
      title: current.titleEdited || current.title ? current.title : titleFrom(text, attachments[0]?.name),
      updatedAt: now,
      columns: current.columns.map((column) => {
        const user: ChatMessage = { id: newID("user"), role: "user", content: text, attachments };
        const reply: ChatMessage = { id: newID("assistant"), role: "assistant", content: "", streaming: true, model: column.model, owner: this.opts.pageID };
        started.push({ column: { ...column, messages: [...column.messages, user] }, message: reply });
        return { ...column, messages: [...column.messages, user, reply] };
      }),
    }), "now");
    for (const { column, message } of started) void this.streams.start(conversation.id, column, message, streamOptions);
    return { status: "sent" };
  }

  private abortColumn(conversationID: string, column: ConversationColumn) {
    this.streams.abortColumn(conversationID, column);
  }

  private streamsChanged(running: number) {
    this.set({ running });
    if (typeof window === "undefined") return;
    if (running) window.addEventListener("beforeunload", this.onBeforeUnload);
    else window.removeEventListener("beforeunload", this.onBeforeUnload);
  }

  private patchMessage(conversationID: string, messageID: string, patch: (message: ChatMessage) => ChatMessage, save: "now" | "throttled") {
    this.update(conversationID, (conversation) => ({
      ...conversation,
      columns: conversation.columns.map((column) => column.messages.some((message) => message.id === messageID)
        ? { ...column, messages: column.messages.map((message) => message.id === messageID ? patch(message) : message) }
        : column),
    }), save);
  }

  private updateColumn(columnID: string, patch: (column: ConversationColumn, conversation: Conversation) => ConversationColumn) {
    const conversation = this.state.conversations.find((candidate) => candidate.columns.some((column) => column.id === columnID));
    if (!conversation) return;
    this.update(conversation.id, (current) => ({
      ...current,
      columns: current.columns.map((column) => column.id === columnID ? patch(column, current) : column),
    }), "throttled");
  }

  private update(id: string, patch: (conversation: Conversation) => Conversation, save: "now" | "throttled" | "none") {
    let changed: Conversation | undefined;
    const conversations = this.state.conversations.map((conversation) => {
      if (conversation.id !== id) return conversation;
      changed = patch(conversation);
      return changed;
    });
    if (!changed) return;
    this.set({ conversations: conversations.sort(newestFirst) });
    if (save === "now") void this.save(id);
    else if (save === "throttled") this.scheduleSave(id);
  }

  private scheduleSave(id: string) {
    if (this.saveTimers.has(id)) return;
    this.saveTimers.set(id, setTimeout(() => { this.saveTimers.delete(id); void this.save(id); }, this.opts.saveIntervalMS));
  }

  private clearSaveTimer(id: string) {
    const timer = this.saveTimers.get(id);
    if (timer !== undefined) clearTimeout(timer);
    this.saveTimers.delete(id);
  }

  /** save writes one conversation; a conversation that never had a message is not kept. */
  private async save(id: string) {
    this.clearSaveTimer(id);
    const conversation = this.state.conversations.find((candidate) => candidate.id === id);
    if (!conversation || !this.db || (isEmpty(conversation) && !this.persisted.has(id))) return;
    const first = !this.persisted.has(id);
    this.persisted.add(id);
    try {
      await this.db.put(conversation);
      this.peers?.post({ type: "put", userID: conversation.userID, id });
      if (first) await this.prune(conversation.userID);
    } catch (error) {
      console.warn("Unable to save playground conversation", error);
    }
  }

  private async prune(userID: number) {
    if (!this.db) return;
    const stored = await this.db.list(userID);
    const own = this.streams.conversations();
    if (this.state.currentID) own.add(this.state.currentID);
    const drop = overflow(stored, own);
    if (!drop.length) return;
    await this.removeStored(drop);
    const gone = new Set(drop);
    drop.forEach((id) => this.persisted.delete(id));
    this.set({ conversations: this.state.conversations.filter((conversation) => !gone.has(conversation.id)) });
  }

  private async removeStored(ids: string[]) {
    try {
      await this.db?.remove(ids);
      if (this.state.userID !== undefined) this.peers?.post({ type: "delete", userID: this.state.userID, ids });
    } catch (error) {
      console.warn("Unable to delete playground conversation", error);
    }
  }

  private async load(userID: number, generation: number) {
    try {
      this.db = await this.opts.openDB();
    } catch (error) {
      console.warn("Playground conversations will not be kept: IndexedDB is unavailable", error);
      this.db = undefined;
    }
    if (generation !== this.generation) return;
    this.peers = this.opts.peers();
    this.peers.listen((message) => void this.onPeerMessage(message, generation));
    let stored: Conversation[] = [];
    try {
      stored = this.db ? await this.db.list(userID) : [];
    } catch (error) {
      console.warn("Unable to read playground conversations", error);
    }
    if (generation !== this.generation) return;
    const legacy = this.opts.session ? takeLegacySessions(this.opts.session, userID, this.opts.now()) : [];
    stored.forEach((conversation) => this.persisted.add(conversation.id));
    const remembered = this.opts.session?.getItem(`${CURRENT_KEY}.${userID}`) ?? undefined;
    const partial = this.opts.session ? takePartialReplies(this.opts.session, userID) : [];
    const conversations = [...legacy, ...stored.map((conversation) => mergePartialReplies(conversation, partial))].sort(newestFirst);
    const currentID = conversations.some((conversation) => conversation.id === remembered) ? remembered : conversations[0]?.id;
    this.set({ status: "ready", persistent: !!this.db, conversations, currentID });
    for (const conversation of legacy) void this.save(conversation.id);
    await this.settleOrphans(conversations, generation);
  }

  /**
   * settleOrphans marks replies left streaming by pages that no longer run as
   * interrupted. It asks the pages that wrote them; one that answers within
   * probeWaitMS is still generating, however long it has gone without a
   * token.
   */
  private async settleOrphans(conversations: Conversation[], generation: number) {
    const owners = new Set(conversations.flatMap(streamingMessages).map((message) => message.owner ?? "").filter((owner) => owner !== this.opts.pageID));
    if (!owners.size) return;
    const nonce = newID("probe");
    const alive = new Set<string>();
    this.probes.set(nonce, alive);
    this.peers?.post({ type: "probe", userID: this.state.userID!, owners: [...owners].filter(Boolean), nonce });
    await new Promise((resolve) => setTimeout(resolve, this.opts.probeWaitMS));
    this.probes.delete(nonce);
    if (generation !== this.generation) return;
    alive.add(this.opts.pageID);
    for (const { id } of conversations) {
      const latest = this.state.conversations.find((candidate) => candidate.id === id);
      if (!latest) continue;
      const settled = markInterrupted(latest, alive);
      if (settled !== latest) this.update(id, () => settled, "now");
    }
  }

  private async onPeerMessage(message: SyncMessage, generation: number) {
    if (generation !== this.generation || message.userID !== this.state.userID) return;
    switch (message.type) {
      case "probe":
        if (message.owners.includes(this.opts.pageID)) this.peers?.post({ type: "alive", userID: message.userID, owner: this.opts.pageID, nonce: message.nonce });
        return;
      case "alive":
        this.probes.get(message.nonce)?.add(message.owner);
        return;
      case "delete": {
        const gone = new Set(message.ids);
        this.streams.abortConversations(gone);
        message.ids.forEach((id) => this.persisted.delete(id));
        const conversations = this.state.conversations.filter((conversation) => !gone.has(conversation.id));
        this.set({ conversations, currentID: gone.has(this.state.currentID ?? "") ? conversations[0]?.id : this.state.currentID });
        return;
      }
      case "put": {
        // This page is the writer while it generates there.
        if (this.streams.conversations().has(message.id)) return;
        const record = await this.db?.get(message.id).catch(() => undefined);
        if (!record || generation !== this.generation) return;
        this.persisted.add(record.id);
        const others = this.state.conversations.filter((conversation) => conversation.id !== record.id);
        this.set({ conversations: [...others, record].sort(newestFirst) });
      }
    }
  }

  // The page may be about to go: save what the replies have so far while the
  // browser asks, so a reload keeps the partial text (marked interrupted by
  // pagehide or by the next page's probe).
  private readonly onBeforeUnload = (event: BeforeUnloadEvent) => {
    event.preventDefault();
    event.returnValue = "";
    this.streams.flushAll();
    for (const id of this.streams.conversations()) void this.save(id);
  };

  // The browser cuts the replies off as the page goes away; record that, so
  // other pages and the next visit show them as interrupted. The IndexedDB
  // write may not finish; the text also goes to sessionStorage, which a
  // reload of this tab merges back, and the next page's probe marks the
  // replies interrupted.
  private readonly onPageHide = () => {
    this.streams.flushAll();
    const own = this.state.conversations.filter((conversation) => this.streams.conversations().has(conversation.id));
    if (this.opts.session && this.state.userID !== undefined) {
      keepPartialReplies(this.opts.session, this.state.userID, own.flatMap((conversation) => streamingMessages(conversation)
        .filter((message) => message.owner === this.opts.pageID)
        .map((message) => ({ conversationID: conversation.id, messageID: message.id, content: message.content, reasoning: message.reasoning }))));
    }
    // Marked in memory too, so a save queued before (or text arriving after)
    // this point cannot write the replies back as streaming.
    for (const conversation of own) this.update(conversation.id, (current) => markInterrupted(current, new Set()), "now");
  };

  private reset() {
    this.generation += 1;
    this.streams.clear();
    for (const timer of this.saveTimers.values()) clearTimeout(timer);
    this.saveTimers.clear();
    this.persisted.clear();
    this.probes.clear();
    this.peers?.close();
    this.peers = undefined;
    this.loading = undefined;
    this.state = { ...this.state, running: 0 };
    if (typeof window !== "undefined") window.removeEventListener("beforeunload", this.onBeforeUnload);
  }

  private rememberCurrent() {
    const { userID, currentID } = this.state;
    if (userID === undefined) return;
    try {
      if (currentID) this.opts.session?.setItem(`${CURRENT_KEY}.${userID}`, currentID);
    } catch {
      // A full or blocked sessionStorage only loses which conversation was open.
    }
  }

  private set(patch: Partial<ConversationState>) {
    this.state = { ...this.state, ...patch };
    for (const listener of this.listeners) listener();
  }
}

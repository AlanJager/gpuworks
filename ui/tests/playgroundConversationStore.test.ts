import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { controllableSSE, dom, originalFetch, serveStreams, testStore } from "./helpers/playgroundChat.tsx";
import { openConversationDB } from "../src/pages/playground/conversations/db.ts";
import { MAX_CONVERSATIONS, newColumn, newConversation, overflow, type Conversation } from "../src/pages/playground/conversations/model.ts";
import type { ConversationStore } from "../src/pages/playground/conversations/store.ts";
import { streamLimit } from "../src/pages/playground/streamLimit.ts";
import type { ParameterState } from "../src/pages/playground/types.ts";

afterEach(() => {
  dom.window.sessionStorage.clear();
  globalThis.fetch = originalFetch;
});

const parameters: ParameterState = { enabled: {}, values: {}, systemPrompt: "", extraJSON: "" };
const defaults = { model: "chat-model", parameters };

async function until(check: () => boolean | Promise<boolean>, what: string, timeoutMS = 10_000) {
  const deadline = Date.now() + timeoutMS;
  while (!(await check())) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
}

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

// A page that opens next to one still generating waits this long for it to
// answer the probe (start() resolves only after the verdict). The helper's
// 20 ms suits tests where nobody answers; with a live peer the probe and its
// answer are two timer hops each, and any event-loop stall of 20 ms (a loaded
// CI runner, GC) made the answer miss the window, so the reply was marked
// interrupted and the page took it for a conversation nobody was generating in.
const LIVE_PEER_PROBE_WAIT_MS = 1_000;

async function ready(store: ConversationStore, userID = 1) {
  await store.start(userID);
  store.ensureConversation(defaults);
  return store;
}

function reply(store: ConversationStore, column = 0) {
  return store.current()!.columns[column].messages.at(-1)!;
}

function record(userID: number, updatedAt: number, patch: Partial<Conversation> = {}): Conversation {
  const conversation = newConversation(userID, "single", [newColumn("chat-model", parameters)], updatedAt);
  conversation.columns[0].messages = [{ id: `u-${updatedAt}`, role: "user", content: `question ${updatedAt}` }, { id: `a-${updatedAt}`, role: "assistant", content: "answer", model: "chat-model" }];
  return { ...conversation, title: `question ${updatedAt}`, ...patch };
}

test("a reply streams on without any page mounted and is saved with the model that answered", async () => {
  const stream = controllableSSE();
  const bodies = serveStreams([stream]);
  const { store, idb, hub } = testStore({ pageID: "page-a" });
  await ready(store);
  assert.equal(store.send("hello", [], []).status, "sent");
  assert.equal(store.getState().running, 1);
  assert.equal(store.current()!.title, "hello");
  stream.push("hi ");
  stream.push("there");
  stream.end();
  await until(() => !reply(store).streaming, "the reply to finish");
  assert.equal(reply(store).content, "hi there");
  assert.equal(reply(store).model, "chat-model");
  assert.equal(store.getState().running, 0);
  assert.deepEqual(bodies[0].messages, [{ role: "user", content: "hello" }]);

  // Another page of the same browser and account reads it back.
  const { store: other } = testStore({ idb, hub, pageID: "page-b" });
  await other.start(1);
  const [saved] = other.getState().conversations;
  assert.equal(saved.title, "hello");
  assert.equal(saved.columns[0].messages[1].content, "hi there");
  assert.equal(saved.columns[0].messages[1].streaming, false);
});

test("conversations are kept per account", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const { store, idb } = testStore();
  await ready(store, 1);
  store.send("mine", [], []);
  stream.end();
  await until(() => !reply(store).streaming, "the reply");

  const { store: second } = testStore({ idb });
  await second.start(2);
  assert.deepEqual(second.getState().conversations, []);
  await second.start(1);
  assert.equal(second.getState().conversations[0].title, "mine");
});

test("signing out stops every reply and forgets the account in the page", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const { store } = testStore();
  await ready(store);
  store.send("long", [], []);
  assert.equal(store.getState().running, 1);
  store.signOut();
  assert.equal(store.getState().running, 0);
  assert.equal(store.getState().status, "idle");
  assert.deepEqual(store.getState().conversations, []);
});

test("only the newest conversations are kept; open and generating ones never drop", () => {
  const stored = Array.from({ length: MAX_CONVERSATIONS + 3 }, (_, index) => record(1, index + 1));
  stored[0] = { ...stored[0], columns: [{ ...stored[0].columns[0], messages: [{ id: "x", role: "assistant", content: "", streaming: true, owner: "page" }] }] };
  const drop = overflow(stored, new Set([stored[1].id]));
  // 63 conversations: the 60 newest stay, plus the generating and the open one.
  assert.deepEqual(drop, [stored[2].id]);
});

test("saving a new conversation drops the oldest beyond the limit", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const { store, idb } = testStore();
  const db = await openConversationDB(idb);
  for (let index = 1; index <= MAX_CONVERSATIONS; index += 1) await db.put(record(1, index));
  await db.put(record(2, 1));
  await store.start(1);
  store.newConversation();
  assert.equal(store.send("one more", [], []).status, "sent");
  stream.end();
  await until(() => store.getState().conversations.length === MAX_CONVERSATIONS, "the oldest to drop");
  const left = await db.list(1);
  assert.equal(left.length, MAX_CONVERSATIONS);
  assert.ok(!left.some((conversation) => conversation.title === "question 1"), "the oldest is gone");
  assert.equal((await db.list(2)).length, 1, "other accounts are untouched");
});

test("a reply left streaming by a page that is gone is marked interrupted", async () => {
  const { store, idb } = testStore({ pageID: "page-new" });
  const db = await openConversationDB(idb);
  const orphan = record(1, 1);
  orphan.columns[0].messages.push({ id: "u2", role: "user", content: "again" }, { id: "a2", role: "assistant", content: "half an ans", streaming: true, owner: "page-reloaded", model: "chat-model" });
  await db.put(orphan);
  await store.start(1);
  await until(() => !!store.getState().conversations[0].columns[0].messages[3].interrupted, "the reply to be marked");
  const settled = store.getState().conversations[0].columns[0].messages[3];
  assert.equal(settled.streaming, false);
  assert.equal(settled.content, "half an ans");
  await until(async () => !!(await db.get(orphan.id))!.columns[0].messages[3].interrupted, "the record to be saved interrupted");
});

test("a page still generating answers the probe, however long the prefill takes", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const { store: generating, idb, hub } = testStore({ pageID: "page-a" });
  await ready(generating);
  generating.send("think hard", [], []);
  // The reply is saved while no token has arrived yet.
  await until(() => hub.sent.some((message) => message.type === "put"), "the first save");

  const { store: reader } = testStore({ idb, hub, pageID: "page-b", probeWaitMS: LIVE_PEER_PROBE_WAIT_MS });
  await reader.start(1);
  await until(() => hub.sent.some((message) => message.type === "alive"), "page-a to answer");
  await sleep(60);
  const waiting = reader.getState().conversations[0].columns[0].messages[1];
  assert.equal(waiting.streaming, true, "still generating, not interrupted");
  assert.notEqual(waiting.interrupted, true);

  stream.push("done thinking");
  stream.end();
  await until(() => reader.getState().conversations[0].columns[0].messages[1].content === "done thinking", "the other page to see the reply");
});

test("a page going away records its replies as interrupted", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const { store, idb } = testStore({ pageID: "page-a" });
  await ready(store);
  store.send("bye", [], []);
  stream.push("partial");
  await until(() => reply(store).content === "partial", "the partial reply");
  window.dispatchEvent(new dom.window.Event("pagehide"));
  // Saves already queued, and text still arriving before the browser cuts
  // the request, must not write the reply back as streaming.
  stream.push(" more");
  await until(() => reply(store).content === "partial more", "the late text");
  const db = await openConversationDB(idb);
  const saved = async () => (await db.get(store.current()!.id))!.columns[0].messages[1];
  // The pagehide save is an IndexedDB write that takes however long it takes ...
  await until(async () => !!(await saved()).interrupted, "the record to be saved interrupted");
  // ... then a save carrying the late text (throttled to 10 ms here) must not
  // write the reply back as streaming.
  await sleep(50);
  const settled = await saved();
  assert.equal(settled.interrupted, true);
  assert.equal(settled.streaming, false);
  stream.end();
});

test("leaving the page while generating asks first, and only then", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const { store } = testStore();
  await ready(store);
  const unload = () => {
    const event = new dom.window.Event("beforeunload", { cancelable: true });
    window.dispatchEvent(event);
    return event.defaultPrevented;
  };
  assert.equal(unload(), false);
  store.send("hi", [], []);
  assert.equal(unload(), true);
  stream.end();
  await until(() => store.getState().running === 0, "the reply to end");
  assert.equal(unload(), false);
});

test("a reload keeps the partial text even when no IndexedDB write landed", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  let dropWrites = false;
  const { store, idb, hub } = testStore({
    pageID: "page-old",
    saveIntervalMS: 60_000,
    openDB: async () => {
      const db = await openConversationDB(idb);
      return { ...db, put: (conversation) => dropWrites ? Promise.resolve() : db.put(conversation) };
    },
  });
  await ready(store);
  store.send("hi", [], []);
  stream.push("partial text");
  await until(() => reply(store).content === "partial text", "the partial reply");
  dropWrites = true;
  window.dispatchEvent(new dom.window.Event("pagehide"));
  store.dispose();

  const { store: reloaded } = testStore({ idb, hub, pageID: "page-new" });
  await reloaded.start(1);
  await until(() => !!reloaded.getState().conversations[0]?.columns[0].messages[1].interrupted, "the reply to be marked");
  assert.equal(reloaded.getState().conversations[0].columns[0].messages[1].content, "partial text");
  assert.equal(dom.window.sessionStorage.getItem("gpuworks.playground.unload.1"), null, "taken once");
});

test("a page about to unload saves the partial reply before the throttle would", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const { store, idb } = testStore({ saveIntervalMS: 60_000 });
  await ready(store);
  store.send("hi", [], []);
  stream.push("partial text");
  await until(() => reply(store).content === "partial text", "the partial reply");
  window.dispatchEvent(new dom.window.Event("beforeunload", { cancelable: true }));
  const db = await openConversationDB(idb);
  await until(async () => (await db.get(store.current()!.id))?.columns[0].messages[1].content === "partial text", "the partial reply to be saved");
  stream.end();
});

test("over HTTP/1.1 at most four replies stream at once; a send that does not fit is refused whole", async () => {
  const streams = Array.from({ length: 6 }, () => controllableSSE());
  const bodies = serveStreams(streams);
  const { store } = testStore({ limit: () => streamLimit("http/1.1") });
  await ready(store);
  store.setMode("compare");
  store.addColumn();
  assert.equal(store.send("three at once", [], []).status, "sent");
  store.newConversation();
  // The new conversation inherits three columns: 3 + 3 > 4.
  assert.deepEqual(store.send("three more", [], []), { status: "limited", need: 3, running: 3, limit: 4 });
  assert.equal(bodies.length, 3, "nothing of the refused send went out");
  store.setMode("single");
  assert.equal(store.send("one more", [], []).status, "sent");
  assert.equal(store.getState().running, 4);
  for (const stream of streams.slice(0, 4)) stream.end();
  await until(() => store.getState().running === 0, "replies to end");
});

test("over HTTP/2 the limit is twelve", () => {
  assert.deepEqual(streamLimit("h2"), { limit: 12, multiplexed: true });
  assert.deepEqual(streamLimit("h3"), { limit: 12, multiplexed: true });
  assert.deepEqual(streamLimit("http/1.1"), { limit: 4, multiplexed: false });
  assert.deepEqual(streamLimit(""), { limit: 4, multiplexed: false });
});

test("sending again in a conversation replaces its own replies instead of counting them twice", async () => {
  const streams = [controllableSSE(), controllableSSE()];
  serveStreams(streams);
  const { store } = testStore({ limit: () => ({ limit: 1, multiplexed: false }) });
  await ready(store);
  store.send("first", [], []);
  assert.equal(store.send("second", [], []).status, "sent");
  await until(() => store.current()!.columns[0].messages[1].error !== undefined, "the first reply to stop");
  assert.equal(store.getState().running, 1);
  streams[1].end();
});

test("a conversation another page is generating in cannot be sent to", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const { store: a, idb, hub } = testStore({ pageID: "page-a" });
  await ready(a);
  a.send("mine", [], []);
  await until(() => hub.sent.some((message) => message.type === "put"), "the first save");
  const { store: b } = testStore({ idb, hub, pageID: "page-b", probeWaitMS: LIVE_PEER_PROBE_WAIT_MS });
  await b.start(1);
  b.open(a.current()!.id);
  assert.deepEqual(b.send("me too", [], []), { status: "busy" });
  stream.end();
});

test("deleting a conversation stops its replies and other pages drop it", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const { store: a, idb, hub } = testStore({ pageID: "page-a" });
  await ready(a);
  a.send("to delete", [], []);
  await until(() => hub.sent.some((message) => message.type === "put"), "the first save");
  const { store: b } = testStore({ idb, hub, pageID: "page-b" });
  await b.start(1);
  assert.equal(b.getState().conversations.length, 1);
  a.remove(a.current()!.id);
  await until(() => a.getState().running === 0, "the reply to stop");
  await until(() => b.getState().conversations.length === 0, "the other page to drop it");
  assert.deepEqual(await (await openConversationDB(idb)).list(1), []);
});

test("without IndexedDB conversations work in memory and say so", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const { store } = testStore({ openDB: () => Promise.reject(new Error("blocked")) });
  await ready(store);
  assert.equal(store.getState().persistent, false);
  assert.equal(store.send("hi", [], []).status, "sent");
  stream.push("ok");
  stream.end();
  await until(() => reply(store).content === "ok", "the reply");
});

test("switching the model keeps the history, and the next reply names the new model", async () => {
  const streams = [controllableSSE(), controllableSSE()];
  const bodies = serveStreams(streams);
  const { store } = testStore();
  await ready(store);
  store.send("one", [], []);
  streams[0].push("from A");
  streams[0].end();
  await until(() => !reply(store).streaming, "the first reply");
  store.setModel(store.current()!.columns[0].id, "other-model");
  store.send("two", [], []);
  assert.equal(bodies[1].model, "other-model");
  assert.deepEqual(bodies[1].messages, [{ role: "user", content: "one" }, { role: "assistant", content: "from A" }, { role: "user", content: "two" }]);
  assert.equal(reply(store).model, "other-model");
  assert.equal(store.current()!.columns[0].messages[1].model, "chat-model");
  streams[1].end();
});

test("an empty conversation switches mode in place; one with messages opens a new conversation", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const { store } = testStore();
  await ready(store);
  const first = store.current()!.id;
  store.setMode("compare");
  assert.equal(store.current()!.id, first);
  assert.equal(store.current()!.columns.length, 2);
  store.addColumn();
  store.addColumn();
  assert.equal(store.current()!.columns.length, 3, "at most three columns");
  store.send("compare", [], []);
  stream.end();
  store.setMode("single");
  assert.notEqual(store.current()!.id, first);
  assert.equal(store.current()!.mode, "single");
  assert.equal(store.getState().conversations.find((conversation) => conversation.id === first)!.columns.length, 3);
  // A new conversation reuses the empty one that is open.
  const empty = store.current()!.id;
  store.newConversation();
  assert.equal(store.current()!.id, empty);
});

test("new conversations inherit the models and parameters of the open one", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const { store } = testStore();
  await ready(store);
  const column = store.current()!.columns[0];
  store.setModel(column.id, "other-model");
  store.setParameters(column.id, { ...parameters, systemPrompt: "be brief" });
  store.send("hi", [], []);
  stream.end();
  store.newConversation();
  assert.equal(store.current()!.columns[0].model, "other-model");
  assert.equal(store.current()!.columns[0].parameters.systemPrompt, "be brief");
  assert.deepEqual(store.current()!.columns[0].messages, []);
});

test("the old playground sessions become conversations once", async () => {
  const session = dom.window.sessionStorage;
  const history = (text: string) => [{ id: `u-${text}`, role: "user", content: text }, { id: `a-${text}`, role: "assistant", content: `re ${text}` }];
  session.setItem("gpuworks.playground.single.0", JSON.stringify({ version: 1, modelID: "a", parameters, conversations: { a: history("on a"), b: history("on b"), c: [] } }));
  session.setItem("gpuworks.playground.compare.0", JSON.stringify({ version: 1, modelID: "a", parameters, conversations: { a: history("left") } }));
  session.setItem("gpuworks.playground.compare.1", JSON.stringify({ version: 1, modelID: "b", parameters, conversations: { b: history("right") } }));
  const { store, idb } = testStore();
  await store.start(1);
  const titles = store.getState().conversations.map((conversation) => `${conversation.mode}:${conversation.title}`).sort();
  assert.deepEqual(titles, ["compare:left", "single:on a", "single:on b"]);
  const compare = store.getState().conversations.find((conversation) => conversation.mode === "compare")!;
  assert.deepEqual(compare.columns.map((column) => column.model), ["a", "b"]);
  assert.equal(compare.columns[1].messages[1].model, "b");
  assert.equal(session.getItem("gpuworks.playground.single.0"), null);
  const db = await openConversationDB(idb);
  await until(async () => (await db.list(1)).length === 3, "the imported conversations to be saved");
});

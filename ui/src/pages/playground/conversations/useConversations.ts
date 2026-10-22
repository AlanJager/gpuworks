import { createContext, useContext, useSyncExternalStore } from "react";
import { ConversationStore, type ConversationState } from "./store";

let pageStore: ConversationStore | undefined;

/** playgroundConversations is the page's one conversation store, created on first use. */
export function playgroundConversations(): ConversationStore {
  pageStore ??= new ConversationStore();
  return pageStore;
}

/** signOutPlayground stops the replies of the account that signs out and forgets its conversations in this page. */
export function signOutPlayground() {
  pageStore?.signOut();
}

/** ConversationStoreContext lets tests hand the page a store of their own. */
export const ConversationStoreContext = createContext<ConversationStore | undefined>(undefined);

export function useConversations(): [ConversationStore, ConversationState] {
  const store = useContext(ConversationStoreContext) ?? playgroundConversations();
  const state = useSyncExternalStore(store.subscribe, store.getState);
  return [store, state];
}
